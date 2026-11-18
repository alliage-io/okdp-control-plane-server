package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	compiler "github.com/okdp/okdp-compiler"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-gitops/catalog"
)

type ServiceVersionsResponse struct {
	Versions []string `json:"versions"`
	Default  string   `json:"default"`
}

// CatalogSource opens the catalogs of the platform: the repositories
// catalogs.yaml names.
type CatalogSource func(ctx context.Context) (*catalog.Catalogs, error)

// ParameterSchemaFile is the schema of a package's parameters.
const ParameterSchemaFile = "config.schema.json"

// DefaultPackageSchemaService reads the packages of the catalogs: their
// versions (the directories of catalog/<service>/), their parameter schema
// (config.schema.json).
type DefaultPackageSchemaService struct {
	contextRepo repository.PlatformRepository
	catalogs    CatalogSource
}

func NewDefaultPackageSchemaService(contextRepo repository.PlatformRepository, catalogs CatalogSource) *DefaultPackageSchemaService {
	return &DefaultPackageSchemaService{contextRepo: contextRepo, catalogs: catalogs}
}

// open indexes the catalogs (okdp-compiler's index: services, versions,
// packages).
func (s *DefaultPackageSchemaService) open(ctx context.Context) (*compiler.Catalog, error) {
	cats := catalog.New()
	if s.catalogs != nil {
		var err error
		if cats, err = s.catalogs(ctx); err != nil {
			return nil, err
		}
	}
	return cats.Index()
}

// menuEntry returns the console menu entry of a service (catalogs.yaml).
func (s *DefaultPackageSchemaService) menuEntry(ctx context.Context, serviceName string) (*models.PlatformService, error) {
	services, err := s.contextRepo.GetPlatformServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get platform services: %w", err)
	}
	for i := range services {
		if services[i].Name == serviceName {
			return &services[i], nil
		}
	}
	return nil, fmt.Errorf("service %q not found in platform services", serviceName)
}

// DefaultVersion is the version a service deploys when none is asked: the
// menu entry's default when the catalog has it, else the highest.
func DefaultVersion(menuDefault string, versions []string) string {
	for _, v := range versions {
		if v == menuDefault {
			return v
		}
	}
	if len(versions) > 0 {
		return versions[0]
	}
	return menuDefault
}

func (s *DefaultPackageSchemaService) GetServiceVersions(ctx context.Context, serviceName string) (*ServiceVersionsResponse, error) {
	entry, err := s.menuEntry(ctx, serviceName)
	if err != nil {
		return nil, err
	}
	versions, err := s.ListPackageTags(ctx, serviceName, "")
	if err != nil {
		slog.Warn(fmt.Sprintf("failed to list the versions of %s in the catalogs", serviceName), "error", err)
		versions = nil
		if entry.DefaultVersion != "" {
			versions = []string{entry.DefaultVersion}
		}
	}
	if versions == nil {
		versions = []string{}
	}
	return &ServiceVersionsResponse{Versions: versions, Default: DefaultVersion(entry.DefaultVersion, versions)}, nil
}

func (s *DefaultPackageSchemaService) ListVersionsForServices(ctx context.Context, services []models.PlatformService) map[string][]string {
	versions := make(map[string][]string, len(services))
	cats, err := s.open(ctx)
	if err != nil {
		slog.Warn("failed to open the catalogs, the services keep their default version", "error", err)
		return versions
	}
	all, err := cats.Services()
	if err != nil {
		slog.Warn("failed to read the catalogs, the services keep their default version", "error", err)
		return versions
	}
	byName := map[string][]string{}
	for _, svc := range all {
		byName[svc.Name] = svc.Versions
	}
	for _, svc := range services {
		if v, ok := byName[svc.Name]; ok {
			versions[svc.Name] = v
		}
	}
	return versions
}

func (s *DefaultPackageSchemaService) CatalogOf(ctx context.Context) map[string]string {
	out := map[string]string{}
	cats, err := s.open(ctx)
	if err != nil {
		return out
	}
	all, err := cats.Services()
	if err != nil {
		return out
	}
	for _, svc := range all {
		out[svc.Name] = svc.Repository
	}
	return out
}

// ListPackageTags lists the versions of a service in the catalogs, highest
// first. repositoryOverride is ignored: a service is in one catalog.
func (s *DefaultPackageSchemaService) ListPackageTags(ctx context.Context, serviceName, repositoryOverride string) ([]string, error) {
	cats, err := s.open(ctx)
	if err != nil {
		return nil, err
	}
	svc, err := cats.Service(serviceName)
	if err != nil {
		return nil, err
	}
	return svc.Versions, nil
}

// resolvePackage returns the files of a package version, defaulting the
// version to the menu default (else the highest).
func (s *DefaultPackageSchemaService) resolvePackage(ctx context.Context, serviceName, tag string) (fs.FS, string, error) {
	cats, err := s.open(ctx)
	if err != nil {
		return nil, "", err
	}
	if tag == "" {
		entry, err := s.menuEntry(ctx, serviceName)
		if err != nil {
			return nil, "", err
		}
		svc, err := cats.Service(serviceName)
		if err != nil {
			return nil, "", err
		}
		tag = DefaultVersion(entry.DefaultVersion, svc.Versions)
	}
	pkg, err := cats.Package(serviceName, tag)
	if err != nil {
		return nil, "", err
	}
	return pkg, tag, nil
}

func (s *DefaultPackageSchemaService) GetParameterSchema(ctx context.Context, serviceName, tag string) (map[string]any, error) {
	pkg, tag, err := s.resolvePackage(ctx, serviceName, tag)
	if err != nil {
		return nil, err
	}
	schema, err := packageSchema(pkg)
	if err != nil {
		return nil, fmt.Errorf("the schema of %s %s: %w", serviceName, tag, err)
	}
	return parameterSchema(schema), nil
}

// GetPackageInputs returns the connections a package declares it needs, so the
// console can offer a choice for each one at deployment time.
func (s *DefaultPackageSchemaService) GetPackageInputs(ctx context.Context, serviceName, tag string) ([]models.PackageInput, error) {
	schema, err := s.GetParameterSchema(ctx, serviceName, tag)
	if err != nil {
		return nil, err
	}
	inputs, err := inputsFromMarkers(schema)
	if err != nil {
		return nil, fmt.Errorf("the connection references of %s %s: %w", serviceName, tag, err)
	}
	return inputs, nil
}

// packageSchema reads config.schema.json of a package: {"type": "object"}
// when it has none (no parameter: the compiler accepts any).
func packageSchema(pkg fs.FS) (map[string]any, error) {
	raw, err := fs.ReadFile(pkg, ParameterSchemaFile)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{"type": "object"}, nil
	}
	if err != nil {
		return nil, err
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", ParameterSchemaFile, err)
	}
	if schema == nil {
		return nil, fmt.Errorf("invalid %s: not a JSON object", ParameterSchemaFile)
	}
	return schema, nil
}

// IsCatalogNotFound reports a service or a version no catalog has.
func IsCatalogNotFound(err error) bool { return errors.Is(err, compiler.ErrNotInCatalog) }

// PlatformKeys are the root properties of a package's parameters that the
// platform fills, never the user: the platform values (global), the
// connections and the named claims, given by the context of the compile.
var PlatformKeys = []string{"global", "connections", "claims"}

// parameterSchema turns a package's config.schema.json into the schema of the
// user parameters: the same document without the reserved root properties
// (PlatformKeys and the dependency keys). The console renders it as the
// deployment form, and submitted parameters are validated against it: with
// the chart's additionalProperties: false, a reserved key is refused. The
// x-ui-* keywords are the chart's own, passed through untouched.
func parameterSchema(chartSchema map[string]any, dependencies ...string) map[string]any {
	reserved := map[string]bool{}
	for _, key := range append(append([]string{}, PlatformKeys...), dependencies...) {
		reserved[key] = true
	}
	result := deepCopyMap(chartSchema)
	if props, ok := result["properties"].(map[string]any); ok {
		for key := range reserved {
			delete(props, key)
		}
	}
	if required, ok := result["required"].([]any); ok {
		kept := make([]any, 0, len(required))
		for _, r := range required {
			if name, _ := r.(string); reserved[name] {
				continue
			}
			kept = append(kept, r)
		}
		result["required"] = kept
	}
	return result
}

func deepCopyMap(src map[string]any) map[string]any {
	raw, _ := json.Marshal(src)
	var dst map[string]any
	_ = json.Unmarshal(raw, &dst)
	return dst
}

// inputsFromMarkers reads the connection inputs a package declares, its
// connection references as the compile reads them (compiler.ConnectionRefs):
// at any depth through properties and items (a Trino catalog list holds
// one per item), never under a combinator or a definition, where the
// compile refuses one. Path is the parameter path of the property
// (hiveCatalogs[].metastore). A root property is also a Parameter the form
// can offer a choice for; a nested one is reported so the console knows
// the contract of the field it renders inside its list.
func inputsFromMarkers(parameters map[string]any) ([]models.PackageInput, error) {
	refs, err := compiler.ConnectionRefs(parameters)
	if err != nil {
		return nil, err
	}
	var inputs []models.PackageInput
	for _, ref := range refs {
		description, _ := ref.Property["description"].(string)
		// A default lets the form say the binding is inherited instead of
		// showing None, which reads as "nothing", the opposite of the truth.
		defaultValue, _ := ref.Property["default"].(string)
		input := models.PackageInput{
			Alias:       ref.Path,
			Path:        ref.Path,
			Contract:    ref.Contract,
			Optional:    !ref.Required,
			Default:     defaultValue,
			Description: description,
			Provision:   ref.Provision,
		}
		if len(ref.Request) > 0 {
			input.Request = ref.Request
		}
		if len(ref.Require) > 0 {
			input.Require = ref.Require
		}
		if claim, ok := ref.Property["default"].(map[string]any); ok && input.Provision {
			template := &models.ClaimTemplate{}
			template.Class, _ = claim["class"].(string)
			template.Provider, _ = claim["provider"].(string)
			template.Request, _ = claim["request"].(map[string]any)
			input.DefaultClaim = template
		}
		if !strings.ContainsAny(ref.Path, ".[") {
			input.Parameter = ref.Path
		}
		inputs = append(inputs, input)
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Path < inputs[j].Path })
	return inputs, nil
}
