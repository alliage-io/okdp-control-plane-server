package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"

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
	// ValidateClaims checks the claims an instance's parameters write or
	// name, along the references of its package's parameter schema: their
	// keys, their class, their request against the contract's claim schema.
	ValidateClaims(ctx context.Context, project string, parameterSchema, params map[string]any) error
}

// DefaultClaimService reads and writes the claims in the deployments
// repository, and binds them with the compile of the tree.
type DefaultClaimService struct {
	deployments *gitops.Deployments
	contracts   ContractCatalog
}

// NewDefaultClaimService serves the claims of deployments, their requests
// checked against contracts.
func NewDefaultClaimService(deployments *gitops.Deployments, contracts ContractCatalog) *DefaultClaimService {
	return &DefaultClaimService{deployments: deployments, contracts: contracts}
}

// providerRef is the form of a provider: an instance or a release, of the
// claimer's project or <namespace>/<instance|release>.
var providerRef = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]*[a-z0-9])?/)?[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

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
		v := models.NamedClaimView{NamedClaim: c, ID: project + "." + c.Name, Users: []string{}}
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

// validateNamedClaim checks a named claim's declaration: its name, its
// contract, its class or provider, its request.
func (s *DefaultClaimService) validateNamedClaim(ctx context.Context, project string, c models.NamedClaim) error {
	if errs := validation.IsDNS1123Label(c.Name); len(errs) > 0 {
		return invalid("invalid claim name %q: %s", c.Name, errs[0])
	}
	if _, err := s.deployments.GetConnection(ctx, project, c.Name); err == nil {
		return invalid("claim %q: a connection of the project has that name", c.Name)
	} else if !errors.Is(err, treeedit.ErrNotFound) {
		return err
	}
	var classes map[string]*models.ContractClasses
	if err := s.deployments.Store.View(ctx, func(r treeedit.Reader) error {
		var err error
		classes, err = s.classes(r, project)
		return err
	}); err != nil {
		return err
	}
	return s.validateClaim("claims."+c.Name, c.Contract, models.ClaimTemplate{Class: c.Class, Provider: c.Provider, Request: c.Request}, nil, classes)
}

// validateClaim checks a claim of a contract: its class or provider, and
// its request (fixed, what the package fixes of every claim of the
// reference, the claim's merged over it) against the contract's claim
// schema and the class.
func (s *DefaultClaimService) validateClaim(where, contract string, c models.ClaimTemplate, fixed map[string]any, classes map[string]*models.ContractClasses) error {
	if s.contracts != nil {
		if _, known := s.contracts.Get(contract); !known {
			return invalid("%s: unknown contract %q", where, contract)
		}
	}
	if c.Class != "" && c.Provider != "" {
		return invalid("%s: a claim names a class or a provider, not both", where)
	}
	cc := classes[contract]
	var class *models.ProvisioningClass
	switch {
	case c.Provider != "":
		if !providerRef.MatchString(c.Provider) {
			return invalid("%s: provider %q: <instance>, <release>, <namespace>/<instance> or <namespace>/<release>", where, c.Provider)
		}
	case c.Class != "":
		if cc != nil {
			for i := range cc.Classes {
				if cc.Classes[i].Name == c.Class {
					class = &cc.Classes[i]
				}
			}
		}
		if class == nil {
			return invalid("%s: class %q is not a class of %s connections (global.okdp.provisioning.%s.classes): %s", where, c.Class, contract, contract, classNames(cc))
		}
	default:
		if cc == nil || cc.Default == "" {
			return invalid("%s: the claim names no class, and %s connections have no default class (global.okdp.provisioning.%s.default): name one of %s", where, contract, contract, classNames(cc))
		}
		for i := range cc.Classes {
			if cc.Classes[i].Name == cc.Default {
				class = &cc.Classes[i]
			}
		}
	}
	request := MergePatch(fixed, c.Request)
	if class != nil {
		var set []string
		for k := range class.Request {
			if _, ok := request[k]; ok {
				set = append(set, k)
			}
		}
		sort.Strings(set)
		if len(set) > 0 {
			return invalid("%s: request.%s: set by the class %s (global.okdp.provisioning.%s.classes.%s): remove it, or name another class", where, set[0], class.Name, contract, class.Name)
		}
		request = MergePatch(class.Request, request)
	}
	if s.contracts != nil {
		if err := s.contracts.ValidateClaimRequest(contract, request); err != nil {
			return invalid("%s: %v", where, err)
		}
	}
	return nil
}

// classNames lists the classes of a contract, for messages.
func classNames(cc *models.ContractClasses) string {
	if cc == nil || len(cc.Classes) == 0 {
		return "none is defined"
	}
	names := make([]string, 0, len(cc.Classes))
	for _, c := range cc.Classes {
		names = append(names, c.Name)
	}
	return strings.Join(names, ", ")
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
	claim := models.NamedClaim{Name: name, Contract: req.Contract, Class: req.Class, Provider: req.Provider, Request: req.Request}
	if _, err := s.deployments.GetProject(ctx, project); errors.Is(err, treeedit.ErrNotFound) {
		return nil, apierrors.NewNotFound(projectsResource, project)
	} else if err != nil {
		return nil, err
	}
	if err := s.validateNamedClaim(ctx, project, claim); err != nil {
		return nil, err
	}
	// Written in project.yaml (okdp-gitops), refused when an instance that
	// compiled before does not after.
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
	return &models.NamedClaimView{NamedClaim: claim, ID: project + "." + name, Users: []string{}}, nil
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
		named[project+"."+c.Name] = c.Name
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
			if strings.Count(pc.ID, ".") == 1 {
				c.Name = strings.TrimPrefix(pc.ID, c.Namespace+".")
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
			if _, seen := index[o.Name]; seen || !provisions[o.Contract] || strings.Count(o.Name, ".") != 1 {
				continue
			}
			namespace, name, _ := strings.Cut(o.Name, ".")
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

// ValidateClaims checks the claims of an instance's parameters, along the
// connection references of its package's parameter schema.
func (s *DefaultClaimService) ValidateClaims(ctx context.Context, project string, parameterSchema, params map[string]any) error {
	type found struct {
		path     string
		contract string
		marker   map[string]any
		value    any
	}
	var refs []found
	walkRefValues(parameterSchema, params, "", func(p string, marker map[string]any, v any) {
		contract, _ := marker["contract"].(string)
		refs = append(refs, found{p, contract, marker, v})
	})
	if len(refs) == 0 {
		return nil
	}
	var classes map[string]*models.ContractClasses
	err := s.deployments.Store.View(ctx, func(r treeedit.Reader) error {
		var err error
		classes, err = s.classes(r, project)
		return err
	})
	if err != nil {
		return err
	}
	claims, err := s.namedClaims(ctx, project)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		provision, _ := ref.marker["provision"].(bool)
		switch v := ref.value.(type) {
		case map[string]any:
			if !provision {
				return invalid("%s: the reference takes no claim (x-okdp-connection-ref's provision): name a connection", ref.path)
			}
			var c models.ClaimTemplate
			for k, value := range v {
				switch k {
				case "class", "provider":
					str, ok := value.(string)
					if !ok {
						return invalid("%s: claim: %s must be a string", ref.path, k)
					}
					if k == "class" {
						c.Class = str
					} else {
						c.Provider = str
					}
				case "request":
					request, ok := value.(map[string]any)
					if !ok && value != nil {
						return invalid("%s: claim: request must be an object", ref.path)
					}
					c.Request = request
				default:
					return invalid("%s: claim: unknown key %q: class, provider and request only", ref.path, k)
				}
			}
			fixed, _ := ref.marker["request"].(map[string]any)
			if err := s.validateClaim(ref.path, ref.contract, c, fixed, classes); err != nil {
				return err
			}
		case string:
			for _, nc := range claims {
				if nc.Name != v {
					continue
				}
				if !provision {
					return invalid("%s: %q is a named claim of the project: the reference takes none (x-okdp-connection-ref's provision)", ref.path, v)
				}
				if nc.Contract != ref.contract {
					return invalid("%s: claim %q has contract %q, %q is expected", ref.path, v, nc.Contract, ref.contract)
				}
			}
		}
	}
	return nil
}

// walkRefValues calls fn on every connection reference of params, along
// the parameter schema: through properties (the keys present) and items
// (every item of a list), as okdp-compiler finds them. path is the
// parameter path (catalogs[1].storage).
func walkRefValues(schemaNode map[string]any, value any, p string, fn func(p string, marker map[string]any, value any)) {
	if marker, ok := schemaNode[ConnectionRefKeyword].(map[string]any); ok {
		fn(p, marker, value)
		return
	}
	switch v := value.(type) {
	case map[string]any:
		props, _ := schemaNode["properties"].(map[string]any)
		keys := make([]string, 0, len(props))
		for k := range props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sub, ok := props[k].(map[string]any)
			item, present := v[k]
			if !ok || !present {
				continue
			}
			next := k
			if p != "" {
				next = p + "." + k
			}
			walkRefValues(sub, item, next, fn)
		}
	case []any:
		items, ok := schemaNode["items"].(map[string]any)
		if !ok {
			return
		}
		for i, item := range v {
			walkRefValues(items, item, fmt.Sprintf("%s[%d]", p, i), fn)
		}
	}
}
