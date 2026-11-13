package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	compiler "github.com/okdp/okdp-compiler"
	"github.com/okdp/okdp-compiler/treeedit"
	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-gitops/gitops"
)

// claimsResource names the resource in not-found and conflict errors.
var claimsResource = schema.GroupResource{Group: "okdp.io", Resource: "claims"}

// ClaimService manages the claims of a project: its named claims (the
// claims: of its project.yaml), the classes a claim may be of (the
// platform's and the project's global.okdp.provisioning), and the claims
// of an instance, bound to their providers by the compile of the tree
// (okdp-gitops' Deployments.Tree).
//
// A claim is a connection a reference asks of the instance that provisions
// its contract (okdp-compiler's claims): written in place of a
// connection's name in an instance's parameters ({}, {class},
// {provider}, with a request), or named, declared once by the project.
type ClaimService interface {
	// ListNamedClaims returns the named claims of a project, each with the
	// provider it is bound to and the instances that name it.
	ListNamedClaims(ctx context.Context, project string) ([]models.NamedClaimView, error)
	// CreateNamedClaim declares a named claim in the project's project.yaml.
	CreateNamedClaim(ctx context.Context, project string, req models.NamedClaimRequest) (*models.NamedClaimView, error)
	// UpdateNamedClaim changes a named claim's declaration.
	UpdateNamedClaim(ctx context.Context, project, name string, req models.NamedClaimRequest) (*models.NamedClaimView, error)
	// DeleteNamedClaim removes a named claim no instance names.
	DeleteNamedClaim(ctx context.Context, project, name string) error
	// Provisioning returns the classes of each contract's claims, the
	// platform's and the project's, with the contract's claim schema.
	Provisioning(ctx context.Context, project string) (*models.ProvisioningResponse, error)
	// InstanceClaims returns the claims of an instance's references and
	// where each is bound; as a provider, the claims bound to it and what
	// it granted each.
	InstanceClaims(ctx context.Context, project, instance string) (*models.InstanceClaims, error)
}

// DefaultClaimService reads and writes the claims in the deployments
// repository, and binds them with the compile of the tree.
type DefaultClaimService struct {
	deployments *gitops.Deployments
	contracts   ContractCatalog
}

// NewDefaultClaimService serves the claims of deployments, the claim
// schemas of contracts shown with their classes. Every claim is checked by
// the compile of the tree that writes it, never here.
func NewDefaultClaimService(deployments *gitops.Deployments, contracts ContractCatalog) *DefaultClaimService {
	return &DefaultClaimService{deployments: deployments, contracts: contracts}
}

// namedClaims returns the named claims of a project, by name.
func (s *DefaultClaimService) namedClaims(ctx context.Context, project string) ([]models.NamedClaim, error) {
	claims, err := s.deployments.ListClaims(ctx, project)
	if err != nil {
		return nil, err
	}
	out := make([]models.NamedClaim, 0, len(claims))
	for _, c := range claims {
		out = append(out, models.NamedClaim{Name: c.Name, Contract: c.Contract, Class: c.Class, Provider: c.Provider, Request: c.Request})
	}
	return out, nil
}

// provisioningOf returns global.okdp.provisioning of a values document.
func provisioningOf(doc map[string]any) map[string]any {
	global, _ := doc["global"].(map[string]any)
	okdp, _ := global["okdp"].(map[string]any)
	p, _ := okdp["provisioning"].(map[string]any)
	return p
}

// readDocument reads a values file of the repository, nil when absent.
func readDocument(r treeedit.Reader, p string) (map[string]any, error) {
	raw, err := r.ReadFile(p)
	if errors.Is(err, treeedit.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	doc, err := treeedit.DecodeValues(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return doc, nil
}

// classes returns the classes of each contract's claims in a project: the
// platform's, then the project's.
func (s *DefaultClaimService) classes(r treeedit.Reader, project string) (map[string]*models.ContractClasses, error) {
	platform, err := readDocument(r, compiler.PlatformFile)
	if err != nil {
		return nil, err
	}
	proj, err := readDocument(r, treeedit.ProjectFilePath(project))
	if err != nil {
		return nil, err
	}
	out := map[string]*models.ContractClasses{}
	add := func(p map[string]any, level string) {
		for contract, raw := range p {
			spec, _ := raw.(map[string]any)
			cc := out[contract]
			if cc == nil {
				cc = &models.ContractClasses{Contract: contract, Classes: []models.ProvisioningClass{}}
				out[contract] = cc
			}
			if def, _ := spec["default"].(string); def != "" && cc.Default == "" {
				cc.Default, cc.DefaultLevel = def, level
			}
			classes, _ := spec["classes"].(map[string]any)
			names := make([]string, 0, len(classes))
			for name := range classes {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				class, _ := classes[name].(map[string]any)
				pc := models.ProvisioningClass{Name: name, Level: level}
				pc.Provider, _ = class["provider"].(string)
				pc.Request, _ = class["request"].(map[string]any)
				cc.Classes = append(cc.Classes, pc)
			}
		}
	}
	add(provisioningOf(platform), models.ClassLevelPlatform)
	add(provisioningOf(proj), models.ClassLevelProject)
	if s.contracts != nil {
		for _, ct := range s.contracts.List() {
			if ct.ClaimSchema == nil {
				continue
			}
			cc := out[ct.Name]
			if cc == nil {
				cc = &models.ContractClasses{Contract: ct.Name, Classes: []models.ProvisioningClass{}}
				out[ct.Name] = cc
			}
			cc.ClaimSchema = ct.ClaimSchema
		}
	}
	return out, nil
}

// Provisioning returns the classes of each contract a claim of the project
// may be of.
func (s *DefaultClaimService) Provisioning(ctx context.Context, project string) (*models.ProvisioningResponse, error) {
	if err := treeedit.ValidateName("project", project); err != nil {
		return nil, apierrors.NewNotFound(projectsResource, project)
	}
	var byContract map[string]*models.ContractClasses
	err := s.deployments.Store.View(ctx, func(r treeedit.Reader) error {
		var err error
		byContract, err = s.classes(r, project)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := &models.ProvisioningResponse{Contracts: []models.ContractClasses{}}
	for _, cc := range byContract {
		out.Contracts = append(out.Contracts, *cc)
	}
	sort.Slice(out.Contracts, func(i, j int) bool { return out.Contracts[i].Contract < out.Contracts[j].Contract })
	return out, nil
}

// tree compiles the tree as it is; one that does not load binds no claim.
func (s *DefaultClaimService) tree(ctx context.Context) (*compiler.Tree, error) {
	t, err := s.deployments.Tree(ctx)
	if _, ok := errors.AsType[*compiler.LoadError](err); ok {
		return &compiler.Tree{}, nil
	}
	return t, err
}

// binding converts a compiler binding.
func binding(t *compiler.Tree, namespace, release, class string, named bool) models.ClaimBinding {
	b := models.ClaimBinding{Namespace: namespace, Release: release, Class: class, Default: !named}
	if ti := t.Release(namespace, release); ti != nil {
		b.Instance = ti.Name
	}
	return b
}

// ListNamedClaims returns the named claims of a project, bound.
func (s *DefaultClaimService) ListNamedClaims(ctx context.Context, project string) ([]models.NamedClaimView, error) {
	if err := treeedit.ValidateName("project", project); err != nil {
		return nil, apierrors.NewNotFound(projectsResource, project)
	}
	if _, err := s.deployments.GetProject(ctx, project); errors.Is(err, treeedit.ErrNotFound) {
		return nil, apierrors.NewNotFound(projectsResource, project)
	} else if err != nil {
		return nil, err
	}
	claims, err := s.namedClaims(ctx, project)
	if err != nil {
		return nil, err
	}
	t, err := s.tree(ctx)
	if err != nil {
		return nil, err
	}
	return s.views(t, project, claims), nil
}

// views binds the named claims of a project in t: the provider of each, the
// instances that name it, why it does not bind. A named claim no instance
// names is bound all the same: its provider publishes its connection.
func (s *DefaultClaimService) views(t *compiler.Tree, project string, claims []models.NamedClaim) []models.NamedClaimView {
	out := make([]models.NamedClaimView, 0, len(claims))
	for _, c := range claims {
		v := models.NamedClaimView{NamedClaim: c, ID: compiler.NamedClaimID(project, c.Name), Users: []string{}}
		for _, ti := range t.Instances {
			if ti.Namespace() == project && ti.Outcome.Err != nil && v.Error == "" &&
				strings.Contains(ti.Outcome.Err.Error(), "claims."+c.Name+":") {
				v.Error = claimError(ti.Outcome.Err.Error(), "claims."+c.Name+":")
			}
			for _, pc := range ti.Provided {
				if pc.ID != v.ID {
					continue
				}
				if v.Binding == nil {
					b := binding(t, pc.Namespace, pc.Release, pc.Class, pc.Named)
					v.Binding = &b
				}
				if pc.Claimer.Namespace() == project && !contains(v.Users, pc.Claimer.Name) {
					v.Users = append(v.Users, pc.Claimer.Name)
				}
			}
		}
		if v.Binding == nil {
			for _, ti := range t.Instances {
				if o := ti.Granted(v.ID); o != nil && o.Contract == c.Contract {
					b := binding(t, ti.Namespace(), ti.Release(), c.Class, c.Class != "" || c.Provider != "")
					v.Binding = &b
				}
			}
			if v.Binding != nil && c.Class == "" && c.Provider == "" {
				v.Binding.Class = s.defaultClass(t, project, c.Contract)
			}
		}
		if v.Binding != nil {
			if p := t.Release(v.Binding.Namespace, v.Binding.Release); p != nil {
				if o := p.Granted(v.ID); o != nil && o.Contract == c.Contract {
					v.Engine, v.Sql = sqlTargetOf(c.Name, c.Contract, o.Values)
				}
			}
		}
		sort.Strings(v.Users)
		out = append(out, v)
	}
	return out
}

// defaultClass returns the default class of a contract's claims in a
// project, as an instance of the project's compile reads it.
func (s *DefaultClaimService) defaultClass(t *compiler.Tree, project, contract string) string {
	for _, ti := range t.Instances {
		if ti.Namespace() != project {
			continue
		}
		for _, b := range ti.Outcome.Claims {
			if b.Contract == contract && !b.Named {
				return b.Class
			}
		}
	}
	return ""
}

// claimError keeps the lines of a compile error about one claim.
func claimError(msg, prefix string) string {
	var lines []string
	for _, line := range strings.Split(msg, "\n") {
		if i := strings.Index(line, prefix); i >= 0 {
			lines = append(lines, strings.TrimSpace(line[i:]))
		}
	}
	return strings.Join(lines, "\n")
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// CreateNamedClaim declares a named claim.
func (s *DefaultClaimService) CreateNamedClaim(ctx context.Context, project string, req models.NamedClaimRequest) (*models.NamedClaimView, error) {
	return s.putNamedClaim(ctx, project, req.Name, req, true)
}

// UpdateNamedClaim changes a named claim's declaration.
func (s *DefaultClaimService) UpdateNamedClaim(ctx context.Context, project, name string, req models.NamedClaimRequest) (*models.NamedClaimView, error) {
	if req.Name != "" && req.Name != name {
		return nil, invalid("a claim's name cannot change: declare a new one")
	}
	return s.putNamedClaim(ctx, project, name, req, false)
}

func (s *DefaultClaimService) putNamedClaim(ctx context.Context, project, name string, req models.NamedClaimRequest, create bool) (*models.NamedClaimView, error) {
	if err := treeedit.ValidateName("project", project); err != nil {
		return nil, apierrors.NewNotFound(projectsResource, project)
	}
	if err := treeedit.ValidateName("claim", name); err != nil {
		return nil, invalid("%v", err)
	}
	claim := models.NamedClaim{Name: name, Contract: req.Contract, Class: req.Class, Provider: req.Provider, Request: req.Request}
	if _, err := s.deployments.GetProject(ctx, project); errors.Is(err, treeedit.ErrNotFound) {
		return nil, apierrors.NewNotFound(projectsResource, project)
	} else if err != nil {
		return nil, err
	}
	// The form of its provider, the compile's rule (compiler.CheckProvider),
	// checked whether or not an instance of the project binds it.
	if claim.Provider != "" {
		if err := compiler.CheckProvider("claims."+name+".provider", claim.Provider); err != nil {
			return nil, rejected(err)
		}
	}
	// Written in project.yaml (okdp-gitops), checked by the compile of the
	// tree: its contract, its class or its provider, its request, as the
	// project's instances bind it, refused when an instance that compiled
	// before does not after (a project without instance binds none).
	decl := compiler.ClaimSpec{Name: name, Contract: claim.Contract, Class: claim.Class, Provider: claim.Provider, Request: claim.Request}
	if _, err := s.deployments.PutClaim(ctx, auth.ActorFrom(ctx), project, decl, create); err != nil {
		return nil, claimGitError(err, name)
	}
	views, err := s.ListNamedClaims(ctx, project)
	if err != nil {
		return nil, err
	}
	for i := range views {
		if views[i].Name == name {
			return &views[i], nil
		}
	}
	return &models.NamedClaimView{NamedClaim: claim, ID: compiler.NamedClaimID(project, name), Users: []string{}}, nil
}

// DeleteNamedClaim removes a named claim, refused while an instance names
// it. Its provider keeps what the claim's connection holds.
func (s *DefaultClaimService) DeleteNamedClaim(ctx context.Context, project, name string) error {
	if err := treeedit.ValidateName("project", project); err != nil {
		return apierrors.NewNotFound(projectsResource, project)
	}
	_, err := s.deployments.DeleteClaim(ctx, auth.ActorFrom(ctx), project, name)
	return claimGitError(err, name)
}

// claimGitError maps the deployments repository errors to the API ones.
func claimGitError(err error, name string) error {
	var inUse *treeedit.ErrInUse
	var compileErr *treeedit.CompileError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &compileErr):
		return DoesNotCompile(compileErr)
	case errors.As(err, &inUse):
		return invalid("claim %q is still named by %s; change them first", name, strings.Join(inUse.Users, ", "))
	case errors.Is(err, treeedit.ErrExists):
		return apierrors.NewAlreadyExists(claimsResource, name)
	case errors.Is(err, treeedit.ErrNotFound):
		return apierrors.NewNotFound(claimsResource, name)
	default:
		return err
	}
}

// InstanceClaims returns the claims of an instance, bound by the compile of
// the project's sources.
func (s *DefaultClaimService) InstanceClaims(ctx context.Context, project, instance string) (*models.InstanceClaims, error) {
	if treeedit.ValidateName("project", project) != nil || treeedit.ValidateName("instance", instance) != nil {
		return nil, apierrors.NewNotFound(servicesResource, instance)
	}
	out := &models.InstanceClaims{Provisions: []string{}, Provided: []models.ProvidedClaim{}, Bound: []models.BoundClaim{}}
	if _, err := s.deployments.GetInstance(ctx, project, instance); errors.Is(err, treeedit.ErrNotFound) {
		return nil, apierrors.NewNotFound(servicesResource, instance)
	} else if err != nil {
		return nil, err
	}
	claims, err := s.namedClaims(ctx, project)
	if err != nil {
		return nil, err
	}
	named := map[string]string{}
	for _, c := range claims {
		named[compiler.NamedClaimID(project, c.Name)] = c.Name
	}
	t, err := s.deployments.Tree(ctx)
	if layout, ok := errors.AsType[*compiler.LoadError](err); ok {
		out.Error = layout.Error()
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	ti := t.Instance(project, instance)
	if ti == nil {
		return out, nil
	}
	if err := ti.Err(); err != nil {
		out.Error = err.Error()
	}
	if p := ti.Provisions(); p != nil {
		out.Provisions = append(out.Provisions, p...)
	}
	out.Provided = provided(ti)
	for _, b := range ti.Outcome.Claims {
		bc := models.BoundClaim{
			Field:    b.Field,
			Contract: b.Contract,
			ID:       b.ID,
			Name:     named[b.ID],
			Binding:  binding(t, b.Namespace, b.Release, b.Class, b.Named),
		}
		if p := t.Release(b.Namespace, b.Release); p != nil {
			bc.Granted = granted(p, b.ID)
		}
		out.Bound = append(out.Bound, bc)
	}
	return out, nil
}

// provided groups the claims bound to a provider by ID: a named claim's
// every claimer, and what the provider granted each claim.
func provided(e *compiler.TreeInstance) []models.ProvidedClaim {
	out := []models.ProvidedClaim{}
	index := map[string]int{}
	for _, pc := range e.Provided {
		i, seen := index[pc.ID]
		if !seen {
			c := models.ProvidedClaim{
				ID:        pc.ID,
				Contract:  pc.Contract,
				Namespace: pc.Claimer.Namespace(),
				Class:     pc.Class,
				Default:   !pc.Named,
				Claimers:  []models.ClaimUser{},
				Granted:   granted(e, pc.ID),
			}
			if _, name, ok := namedClaimOf(pc.ID); ok {
				c.Name = name
			}
			out = append(out, c)
			i = len(out) - 1
			index[pc.ID] = i
		}
		out[i].Claimers = append(out[i].Claimers, models.ClaimUser{
			Namespace: pc.Claimer.Namespace(),
			Instance:  pc.Claimer.Name,
			Release:   pc.Claimer.Release(),
			Field:     pc.Field,
		})
	}
	// The named claims no instance names: bound to the provider all the
	// same, which publishes their connections.
	if e.Outcome.Result != nil {
		provisions := map[string]bool{}
		for _, c := range e.Provisions() {
			provisions[c] = true
		}
		for _, o := range e.Outcome.Result.Info.Provides {
			namespace, name, named := namedClaimOf(o.Name)
			if _, seen := index[o.Name]; seen || !provisions[o.Contract] || !named {
				continue
			}
			out = append(out, models.ProvidedClaim{
				ID: o.Name, Contract: o.Contract, Namespace: namespace, Name: name,
				Claimers: []models.ClaimUser{}, Granted: granted(e, o.Name),
			})
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	}
	return out
}

// granted returns what a provider granted a claim, nil when it publishes
// nothing for it.
func granted(e *compiler.TreeInstance, id string) *models.GrantedConnection {
	o := e.Granted(id)
	if o == nil {
		return nil
	}
	g := &models.GrantedConnection{Values: o.Values}
	if o.SecretRef != nil {
		g.SecretRef = o.SecretRef.Name
	}
	return g
}

// namedClaimOf splits the ID of a named claim, <project>.<name>
// (compiler.NamedClaimID): ok false for another output, a claim of an
// instance's field (<namespace>.<instance>.<field>) or an output named
// after its instance.
func namedClaimOf(id string) (project, name string, ok bool) {
	if !compiler.IsClaimID(id) {
		return "", "", false
	}
	project, name, _ = strings.Cut(id, ".")
	return project, name, compiler.NamedClaimID(project, name) == id && compiler.IsName(name)
}
