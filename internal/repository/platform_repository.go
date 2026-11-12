package repository

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	compiler "github.com/okdp/okdp-compiler"
	"github.com/okdp/okdp-compiler/treeedit"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository/provisioning"
	"github.com/okdp/okdp-gitops/gitops"
	"github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// PlatformRepository reads the platform configuration: the platform values
// every instance receives under global.okdp (platform.yaml in Git), and the
// console service catalog (catalogs.yaml in Git).
type PlatformRepository interface {
	Invalidate()
	// GetPlatformServices returns the managed OKDP services of the console
	// menu (catalogs.yaml categories[].services); their versions are the
	// catalogs' (PackageSchemaService).
	GetPlatformServices(ctx context.Context) ([]models.PlatformService, error)

	// GetMenuCategories returns the console navigation sections (catalogs.yaml categories).
	GetMenuCategories(ctx context.Context) ([]models.MenuCategory, error)

	// GetIngressSuffix returns the ingress domain suffix (global.okdp.ingress.suffix).
	GetIngressSuffix(ctx context.Context) (string, error)

	// GetIdentity returns how OAuth clients are provisioned on this platform
	// (global.okdp.oidc.clientProvisioning).
	GetIdentity(ctx context.Context) (*models.PlatformIdentity, error)

	// GetIdentityOidcConfig returns the OIDC client the console UI should
	// authenticate with (global.okdp.identity.oidc, nil when the platform
	// values do not publish one).
	GetIdentityOidcConfig(ctx context.Context) (*models.IdentityOidcConfig, error)

	// GetOidcIssuer returns the issuer whose tokens the API accepts, "" when the platform names none.
	GetOidcIssuer(ctx context.Context) (string, error)

	// GetOidcClientID returns the client a token must be issued for, "" when the platform names none.
	GetOidcClientID(ctx context.Context) (string, error)

	// GetOidcInsecureSkipVerify reports whether the issuer's certificate is taken on trust (global.okdp.oidc.insecureSkipVerify).
	GetOidcInsecureSkipVerify(ctx context.Context) (bool, error)

	// GetIdentityProvisioningProvider returns the OIDC client provisioning backend
	// (global.okdp.identity.provisioning.provider, "" when unset, meaning none).
	GetIdentityProvisioningProvider(ctx context.Context) (string, error)

	// GetProfileImages returns available images per profile type from global.okdp.jupyter.profiles.
	GetProfileImages(ctx context.Context) (map[string][]models.ProfileImage, error)

	// GetSparkConfig returns Spark operator configuration from global.okdp.sparkOperator.
	GetSparkConfig(ctx context.Context) (*models.SparkConfig, error)
}

// platformValuesCacheTTL bounds how stale the platform values may be. They
// change rarely, and every page reads several of them.
const platformValuesCacheTTL = 5 * time.Second

// platformRepository reads the platform values and the catalog from Git:
// platform.yaml, the context every instance is compiled with, and
// catalogs.yaml.
type platformRepository struct {
	deployments *gitops.Deployments

	mu       sync.Mutex
	cached   map[string]interface{}
	cachedAt time.Time
}

// NewPlatformRepository reads the platform values and the catalog of
// deployments.
func NewPlatformRepository(deployments *gitops.Deployments) PlatformRepository {
	return &platformRepository{deployments: deployments}
}

// menu reads the console menu of catalogs.yaml as the compile reads it
// (treeedit.ParseCatalogs, strictly: a key it does not know, a service's
// versions or repository among them, refused).
func (r *platformRepository) menu(ctx context.Context) ([]treeedit.MenuCategory, error) {
	doc, err := r.catalog(ctx)
	if err != nil {
		return nil, err
	}
	c, err := treeedit.ParseCatalogs(doc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", treeedit.CatalogsFile, err)
	}
	return c.Categories, nil
}

// platformServiceFromMenu is the PlatformService of a menu entry of the
// section category: its versions are the catalogs' (filled by the service
// layer), the menu's default the only one known here.
func platformServiceFromMenu(m treeedit.MenuService, category string) models.PlatformService {
	svc := models.PlatformService{
		Name:           m.Name,
		DefaultVersion: m.Default,
		Description:    m.Description,
		Category:       category,
		Label:          m.Label,
		ExposesUI:      m.ExposesUI,
	}
	if svc.DefaultVersion != "" {
		svc.Versions = []string{svc.DefaultVersion}
	}
	return svc
}

func (r *platformRepository) GetPlatformServices(ctx context.Context) ([]models.PlatformService, error) {
	cats, err := r.menu(ctx)
	if err != nil {
		return nil, err
	}
	if len(cats) == 0 {
		logrus.Warn("No categories found in " + treeedit.CatalogsFile)
	}
	// Never nil: an empty catalog answers [] in JSON, not null.
	services := make([]models.PlatformService, 0)
	for _, cat := range cats {
		for _, m := range cat.Services {
			services = append(services, platformServiceFromMenu(m, cat.Title))
		}
	}
	return services, nil
}

func (r *platformRepository) GetMenuCategories(ctx context.Context) ([]models.MenuCategory, error) {
	cats, err := r.menu(ctx)
	if err != nil {
		return nil, err
	}
	categories := make([]models.MenuCategory, 0, len(cats))
	for i, cat := range cats {
		categories = append(categories, models.MenuCategory{
			Key:   cat.Title,
			Label: cat.Title,
			Icon:  cat.Icon,
			Order: i + 1,
		})
	}
	return categories, nil
}

func (r *platformRepository) GetIngressSuffix(ctx context.Context) (string, error) {
	v, err := r.values(ctx)
	if err != nil {
		return "", err
	}
	suffix, _, _ := unstructured.NestedString(v, "ingress", "suffix")
	if suffix == "" {
		return "", fmt.Errorf("ingress.suffix not found in the platform values (global.okdp)")
	}
	return suffix, nil
}

// GetIdentity reads oidc.clientProvisioning. An absent block means the platform
// was never told, and the safe reading is that the clients already exist in
// Keycloak (existing).
func (r *platformRepository) GetIdentity(ctx context.Context) (*models.PlatformIdentity, error) {
	v, err := r.values(ctx)
	if err != nil {
		return nil, err
	}
	provisioning, _, _ := unstructured.NestedString(v, "oidc", "clientProvisioning")
	if provisioning == "" {
		// Legacy path, kept while platform values migrate to the single oidc block.
		provisioning, _, _ = unstructured.NestedString(v, "identity", "clientProvisioning")
	}
	identity := &models.PlatformIdentity{ClientProvisioning: provisioning}
	if identity.ClientProvisioning == "" {
		identity.ClientProvisioning = models.ClientProvisioningExisting
	}
	return identity, identity.Validate()
}

func (r *platformRepository) GetIdentityOidcConfig(ctx context.Context) (*models.IdentityOidcConfig, error) {
	v, err := r.values(ctx)
	if err != nil {
		return nil, err
	}
	authority, _, _ := unstructured.NestedString(v, "identity", "oidc", "authority")
	clientID, _, _ := unstructured.NestedString(v, "identity", "oidc", "clientId")
	if authority == "" || clientID == "" {
		return nil, nil
	}
	scope, _, _ := unstructured.NestedString(v, "identity", "oidc", "scope")
	return &models.IdentityOidcConfig{
		Authority: authority,
		ClientID:  clientID,
		Scope:     scope,
	}, nil
}

// GetOidcIssuer reads the issuer from the platform values, so a platform declaring its
// identity provider does not repeat it in the server's environment.
func (r *platformRepository) GetOidcIssuer(ctx context.Context) (string, error) {
	v, err := r.values(ctx)
	if err != nil {
		return "", err
	}
	if authority, _, _ := unstructured.NestedString(v, "identity", "oidc", "authority"); authority != "" {
		return authority, nil
	}
	issuer, _, _ := unstructured.NestedString(v, "oidc", "issuerUri")
	return issuer, nil
}

// GetOidcClientID mirrors GetOidcIssuer's two layouts.
func (r *platformRepository) GetOidcClientID(ctx context.Context) (string, error) {
	v, err := r.values(ctx)
	if err != nil {
		return "", err
	}
	if clientID, _, _ := unstructured.NestedString(v, "oidc", "clientId"); clientID != "" {
		return clientID, nil
	}
	clientID, _, _ := unstructured.NestedString(v, "identity", "oidc", "clientId")
	return clientID, nil
}

// GetOidcInsecureSkipVerify reads the platform's answer to a self-signed issuer
// certificate, the setting the Keycloak provisioner already honors.
func (r *platformRepository) GetOidcInsecureSkipVerify(ctx context.Context) (bool, error) {
	v, err := r.values(ctx)
	if err != nil {
		return false, err
	}
	insecure, _, _ := unstructured.NestedBool(v, "oidc", "insecureSkipVerify")
	return insecure, nil
}

// GetIdentityProvisioningProvider names the platform's OAuth client
// provisioning backend (global.okdp.identity.provisioning.provider), for the
// capabilities. Unset means none. The control plane itself provisions and
// deletes no client: with oidc.clientProvisioning existing or dcr, the
// clients are made in Keycloak beforehand or by the packages themselves.
func (r *platformRepository) GetIdentityProvisioningProvider(ctx context.Context) (string, error) {
	v, err := r.values(ctx)
	if err != nil {
		return "", err
	}
	provider, _, _ := unstructured.NestedString(v, "identity", "provisioning", "provider")
	if provider != "" {
		return provider, nil
	}
	return provisioning.ProviderNone, nil
}

func (r *platformRepository) GetProfileImages(ctx context.Context) (map[string][]models.ProfileImage, error) {
	v, err := r.values(ctx)
	if err != nil {
		return nil, err
	}

	result := make(map[string][]models.ProfileImage)
	profileTypes := []string{"jupyterlab", "vscode", "rstudio"}

	for _, pType := range profileTypes {
		rawImages, found, err := unstructured.NestedSlice(v, "jupyter", "profiles", pType, "images")
		if err != nil || !found {
			continue
		}

		var images []models.ProfileImage
		for _, raw := range rawImages {
			m, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			images = append(images, models.ProfileImage{
				Label: getString(m, "label"),
				Image: getString(m, "image"),
			})
		}
		if len(images) > 0 {
			result[pType] = images
		}
	}
	return result, nil
}

func (r *platformRepository) GetSparkConfig(ctx context.Context) (*models.SparkConfig, error) {
	v, err := r.values(ctx)
	if err != nil {
		return nil, err
	}

	sparkOp, found, err := unstructured.NestedMap(v, "sparkOperator")
	if err != nil {
		return nil, fmt.Errorf("failed to read sparkOperator from the platform values: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("sparkOperator not found in the platform values (global.okdp)")
	}

	cfg := &models.SparkConfig{}

	if imgMap, ok := sparkOp["image"].(map[string]interface{}); ok {
		cfg.Image = models.SparkConfigImage{
			Registry:   getString(imgMap, "registry"),
			Repository: getString(imgMap, "repository"),
			Tag:        getString(imgMap, "tag"),
		}
	}

	if sparkMap, ok := sparkOp["spark"].(map[string]interface{}); ok {
		cfg.Spark.DefaultVersion = getString(sparkMap, "defaultVersion")

		if rawImages, ok := sparkMap["images"].([]interface{}); ok {
			for _, raw := range rawImages {
				m, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				cfg.Spark.Images = append(cfg.Spark.Images, models.SparkImage{
					Label: getString(m, "label"),
					Image: getString(m, "image"),
				})
			}
		}

		if defaultsMap, ok := sparkMap["defaults"].(map[string]interface{}); ok {
			if driverMap, ok := defaultsMap["driver"].(map[string]interface{}); ok {
				cfg.Spark.Defaults.Driver = models.ResourceDefaults{
					Cores:  getInt(driverMap, "cores"),
					Memory: getString(driverMap, "memory"),
				}
			}
			if execMap, ok := defaultsMap["executor"].(map[string]interface{}); ok {
				cfg.Spark.Defaults.Executor = models.ExecutorDefaults{
					ResourceDefaults: models.ResourceDefaults{
						Cores:  getInt(execMap, "cores"),
						Memory: getString(execMap, "memory"),
					},
					Instances: getInt(execMap, "instances"),
				}
			}
		}
	}

	return cfg, nil
}

// values returns the global.okdp map of the platform values.
func (r *platformRepository) values(ctx context.Context) (map[string]interface{}, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cached != nil && time.Since(r.cachedAt) < platformValuesCacheTTL {
		return r.cached, nil
	}

	raw, source, err := r.readPlatformValues(ctx)
	if err != nil {
		return nil, err
	}
	doc, err := treeedit.DecodeValues(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	okdp, found, err := unstructured.NestedMap(doc, "global", "okdp")
	if err != nil || !found {
		return nil, fmt.Errorf("%s carries no global.okdp", source)
	}
	r.cached = okdp
	r.cachedAt = time.Now()
	return okdp, nil
}

func (r *platformRepository) readPlatformValues(ctx context.Context) ([]byte, string, error) {
	if r.deployments == nil {
		return nil, "", errors.New("no platform values: no deployments repository configured")
	}
	raw, err := r.deployments.ReadPlatformValues(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("no platform values: %s: %w", compiler.PlatformFile, err)
	}
	return raw, compiler.PlatformFile, nil
}

// catalog returns catalogs.yaml, read as the compiler reads it (a
// serviceCatalog wrapper is refused by the compile of the tree).
func (r *platformRepository) catalog(ctx context.Context) (map[string]interface{}, error) {
	if r.deployments == nil {
		return nil, errors.New("no deployments repository configured")
	}
	doc, err := r.deployments.ReadCatalog(ctx)
	if err != nil {
		if errors.Is(err, treeedit.ErrNotFound) {
			return map[string]interface{}{}, nil
		}
		return nil, err
	}
	return doc, nil
}

func (r *platformRepository) Invalidate() {
	r.mu.Lock()
	r.cached = nil
	r.mu.Unlock()
}

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func getInt(m map[string]interface{}, key string) int {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case int64:
			return int(n)
		case float64:
			return int(n)
		case int:
			return n
		}
	}
	return 0
}
