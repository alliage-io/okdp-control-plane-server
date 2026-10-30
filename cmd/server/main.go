package main

import (
	"context"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/okdp/okdp-gitops/catalog"
	"github.com/okdp/okdp-gitops/charts"
	"github.com/okdp/okdp-gitops/compile"
	"github.com/okdp/okdp-gitops/gitops"
	"github.com/sirupsen/logrus"

	"github.com/okdp/okdp-control-plane-server/internal/api/handlers"
	"github.com/okdp/okdp-control-plane-server/internal/api/router"
	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/buildinfo"
	"github.com/okdp/okdp-control-plane-server/internal/config"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/provisioning"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

// @title           OKDP Server New API
// @version         1.0
// @description     Minimal API server for OKDP UI New
// @termsOfService  http://swagger.io/terms/

// @contact.name    API Support
// @contact.url     http://www.swagger.io/support
// @contact.email   support@swagger.io

// @license.name    Apache 2.0
// @license.url     http://www.apache.org/licenses/LICENSE-2.0.html

// @host            localhost:8093
// @BasePath        /

// @securityDefinitions.apikey BearerAuth
// @in                         header
// @name                       Authorization
// @description                OIDC access token, as "Bearer <token>". Required on
// @description                every /api route except /api/capabilities.

// @security                   BearerAuth
func main() {
	// Load Configuration
	cfg, err := config.Load()
	if err != nil {
		logrus.Fatalf("Failed to load configuration: %v", err)
	}

	// Initialize Logger
	level, err := logrus.ParseLevel(cfg.LogLevel)
	if err != nil {
		level = logrus.InfoLevel
	}
	logrus.SetLevel(level)
	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})

	// Initialize Kubernetes Clients
	k8sClient, err := repository.InitK8sClient()
	if err != nil {
		logrus.Fatalf("Failed to initialize Kubernetes client: %v", err)
	}

	k8sTypedClient, err := repository.InitK8sTypedClient()
	if err != nil {
		logrus.Fatalf("Failed to initialize typed Kubernetes client: %v", err)
	}

	k8sDiscoveryClient, err := repository.InitK8sDiscoveryClient()
	if err != nil {
		logrus.Fatalf("Failed to initialize Kubernetes discovery client: %v", err)
	}

	// The deployments Git repositories, the only desired-state store: the
	// platform, the projects and the output, where the topology says. The
	// console commits the sources (platform/, projects/) only; a CI running
	// okdp-gitops compile on each of their commits writes the output. The
	// commits a user authors name the server in their co-author trailer.
	gitops.Program = buildinfo.Program
	if err := cfg.GitOps.Validate(); err != nil {
		logrus.Fatalf("Invalid GitOps configuration: %v", err)
	}
	topology, err := cfg.GitOps.Topology()
	if err != nil {
		logrus.Fatalf("Invalid GitOps topology: %v", err)
	}
	deployments, err := buildDeployments(cfg.GitOps, topology)
	if err != nil {
		logrus.Fatalf("Failed to prepare the deployments repositories: %v", err)
	}
	// The packages come from the catalogs platform/catalog.yaml names, git
	// repositories fetched at their ref, on the host of the platform's
	// repository with its credentials. Every write compiles the instances it
	// changes, as a check: the packages with okdp-compiler, checked against
	// their upstream charts (the chart cache), the plain charts with their
	// values layers.
	catalogs := &catalog.Loader{Auth: gitops.CatalogAuth(topology.Platform.URL, cfg.GitOps.CredentialsDir, cfg.GitOps.InsecureIgnoreHostKey)}
	chartCache := charts.NewCache(cfg.GitOps.ChartCacheDir, charts.NewRepositoryPuller(cfg.InsecureOCIRegistries))
	compiler := compile.New(catalogs)
	compiler.Charts = chartCache
	deployments.Compiler = compiler
	catalogSource := compile.CatalogSource(deployments, catalogs)
	var engine repository.EngineAdapter
	switch cfg.GitOps.Engine {
	case config.EngineArgoCD:
		engine = repository.NewArgoCDAdapter(k8sClient, cfg.GitOps.ArgoCDNamespace)
	default:
		engine = repository.NewFluxAdapter(k8sClient, cfg.GitOps.ReleasesNamespace)
	}
	// What each instance publishes: its okdp.instance.yaml in the output.
	descriptors := repository.NewDescriptorRepository(deployments)

	// Initialize Project stack: projects are the projects/<p>/project.yaml
	// files of the deployments repository; the console also creates (and
	// deletes) the Namespace of the projects it creates.
	projectNamespaces := repository.NewProjectNamespaceRepository(k8sTypedClient)
	projectService := service.NewDefaultProjectService(projectNamespaces, deployments)
	projectHandler := handlers.NewProjectHandler(projectService)

	// Platform configuration: the platform values (global.okdp) and the
	// service catalog (platform/catalog.yaml). Shared by capabilities, catalog
	// and Spark.
	contextRepo := repository.NewPlatformRepository(k8sTypedClient, cfg.GitOps.ReleasesNamespace, deployments)
	catalogWriter := repository.NewCatalogWriterRepository(deployments)

	// Initialize Identity stack: users and groups in the platform Keycloak
	// realm, through the Admin REST API (KEYCLOAK_*). The realm is located by
	// KEYCLOAK_URL/KEYCLOAK_REALM, else by the issuer the API verifies tokens
	// against.
	identityRepo := repository.NewKeycloakIdentityRepository(cfg, keycloakIssuerSource(cfg, contextRepo))
	identityService := service.NewDefaultIdentityService(identityRepo)
	identityHandler := handlers.NewIdentityHandler(identityService)
	if cfg.KeycloakClientSecret == "" {
		logrus.Info("KEYCLOAK_CLIENT_SECRET is not set: user and group management (/api/v1/identity) is disabled")
	} else {
		logrus.WithFields(logrus.Fields{
			"url":      cfg.KeycloakURL,
			"realm":    cfg.KeycloakRealm,
			"clientId": cfg.KeycloakClientID,
		}).Info("User and group management backed by the Keycloak Admin API (an empty url/realm follows the platform issuer)")
	}

	// Initialize Capabilities stack (platform features derived from the
	// platform values). The identity capability answers on the same condition
	// as the identity routes.
	capabilityService := service.NewDefaultCapabilityService(contextRepo, identityRepo.Available)
	capabilitiesHandler := handlers.NewCapabilitiesHandler(capabilityService)

	// Initialize SecretStore stack (namespace is dynamic per project)
	secretStoreRepo := repository.NewSecretStoreRepository(k8sClient, k8sDiscoveryClient)
	secretStoreService := service.NewDefaultSecretStoreService(secretStoreRepo)
	secretStoreHandler := handlers.NewSecretStoreHandler(secretStoreService)

	// Initialize ExternalSecret stack (namespace is dynamic per project)
	externalSecretRepo := repository.NewExternalSecretRepository(k8sClient, k8sDiscoveryClient)
	externalSecretService := service.NewDefaultExternalSecretService(externalSecretRepo, secretStoreRepo)
	externalSecretHandler := handlers.NewExternalSecretHandler(externalSecretService)

	// Initialize Service stack (instances in Git, status from the cluster)
	schemaService := service.NewDefaultPackageSchemaService(contextRepo, catalogSource)
	// OIDC client provisioning (backend selected per call from the platform values)
	oidcProvisioner := provisioning.NewContextSelector(contextRepo, k8sClient)
	serviceService := service.NewDefaultServiceService(service.ServiceDeps{
		Deployments:     deployments,
		PlatformRepo:    contextRepo,
		CatalogWriter:   catalogWriter,
		SchemaService:   schemaService,
		OidcProvisioner: oidcProvisioner,
		Engine:          engine,
		Descriptors:     descriptors,
		K8sClient:       k8sClient,
		TypedClient:     k8sTypedClient,
		SidecarPrefixes: cfg.ExcludedSidecarPrefixes,
	})
	serviceHandler := handlers.NewServiceHandler(serviceService, schemaService)

	// Initialize Spark stack (SparkApplication CRUD)
	sparkRepo := repository.NewSparkAppRepository(k8sClient)
	sparkService := service.NewDefaultSparkService(sparkRepo, contextRepo, k8sTypedClient)
	sparkHandler := handlers.NewSparkHandler(sparkService)

	// Initialize Connection stack (external connections declared in Git +
	// internal ones published by the instances deployed in a project)
	// The contracts are the catalogs' (contracts/ of each repository).
	contractCatalog := service.NewCatalogContracts(func(ctx context.Context) (fs.FS, error) {
		cats, err := catalogSource(ctx)
		if err != nil {
			return nil, err
		}
		return cats.Contracts()
	})
	connectionSecrets := repository.NewConnectionSecretRepository(k8sTypedClient)
	connectionService := service.NewDefaultConnectionService(deployments, connectionSecrets, descriptors, serviceService.ListServices, contractCatalog)
	connectionHandler := handlers.NewConnectionHandler(connectionService)

	// The identity block is checked once, at startup. A platform whose
	// provisioning backend is misconfigured would deploy services fine and fail
	// much later, when a deleted service leaves its OIDC client behind.
	checkIdentityConfiguration(context.Background(), contextRepo)

	verifier := buildTokenVerifier(context.Background(), cfg, contextRepo)

	r := router.SetupRouter(cfg, verifier, capabilitiesHandler, projectHandler, identityHandler, secretStoreHandler, externalSecretHandler, serviceHandler, sparkHandler, connectionHandler)

	// Start Server
	//
	// Not r.Run: it leaves every timeout at zero, so a client that opens a
	// connection and sends its headers one byte at a time holds a goroutine
	// indefinitely, and an idle keep-alive is never reclaimed.
	//
	// WriteTimeout stays unset on purpose. Three routes stream Server-Sent
	// Events (projects, services, spark apps), and a write deadline would cut
	// them mid-flight, which is exactly what they are for.
	server := &http.Server{
		Addr:              ":" + cfg.ServerPort,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	logrus.WithField("port", cfg.ServerPort).WithField("version", buildinfo.CurrentVersion()).Info("Starting server")
	if err := server.ListenAndServe(); err != nil {
		logrus.Fatalf("Failed to start server: %v", err)
	}
}

// checkIdentityConfiguration fails fast on an identity block that cannot hold,
// and stays quiet otherwise. Platform values that cannot be read at all are not
// fatal: they may simply not be there yet on a fresh cluster, and the routes
// that need them already report their own absence.
func checkIdentityConfiguration(ctx context.Context, contextRepo repository.PlatformRepository) {
	identity, err := contextRepo.GetIdentity(ctx)
	if err != nil {
		// A block that is present and wrong is a configuration error worth
		// stopping for. The two are told apart by the error the repository
		// returns, which only validates what it found.
		if identity != nil {
			logrus.Fatalf("Invalid oidc client-provisioning configuration: %v", err)
		}
		logrus.WithError(err).Warn("Could not read oidc.clientProvisioning, assuming clients are provisioned elsewhere")
		return
	}

	// The check follows the field the provisioner actually resolves from, so a
	// platform accepted here is a platform whose clients really get unmade.
	provider, err := contextRepo.GetIdentityProvisioningProvider(ctx)
	if err != nil {
		logrus.WithError(err).Warn("Could not read identity.provisioning.provider, assuming clients are provisioned elsewhere")
		return
	}

	// Every provider is checked here: an unsupported value or an incomplete
	// Keycloak block would otherwise start fine and fail at the
	// first client cleanup, leaving stale OIDC clients behind.
	switch provider {
	case "", provisioning.ProviderNone:
	case provisioning.ProviderKeycloak:
		if _, err := contextRepo.GetKeycloakProvisioningConfig(ctx); err != nil {
			logrus.Fatalf("identity.provisioning.provider is %q but its configuration is unusable: %v",
				provisioning.ProviderKeycloak, err)
		}
	default:
		logrus.Fatalf("identity.provisioning.provider is %q, which is not supported. Use %q or %q.",
			provider, provisioning.ProviderNone, provisioning.ProviderKeycloak)
	}

	logrus.WithField("provisioningProvider", provider).Info("Identity configuration accepted")
}

// keycloakIssuerSource locates the Keycloak realm when KEYCLOAK_URL or
// KEYCLOAK_REALM is unset: it is the issuer the API trusts (OIDC_ISSUER, else
// the platform values), with the platform's oidc.insecureSkipVerify.
func keycloakIssuerSource(cfg *config.Config, contextRepo repository.PlatformRepository) repository.KeycloakIssuerSource {
	return func(ctx context.Context) (string, bool, error) {
		issuer, err := auth.ResolveIssuer(ctx, cfg.OIDC.Issuer, contextRepo.GetOidcIssuer)
		if err != nil {
			return "", false, err
		}
		insecure, err := contextRepo.GetOidcInsecureSkipVerify(ctx)
		if err != nil {
			insecure = false
		}
		return issuer, insecure, nil
	}
}

// buildTokenVerifier resolves the issuer the API will trust, or returns nil
// when the deployment asked to run without verification.
func buildTokenVerifier(ctx context.Context, cfg *config.Config, contextRepo repository.PlatformRepository) auth.Verifier {
	if cfg.OIDC.Disabled {
		logrus.Warn("AUTH_DISABLED is set: the API accepts unauthenticated requests. Never do this outside local development.")
		return nil
	}

	issuer, err := auth.ResolveIssuer(ctx, cfg.OIDC.Issuer, contextRepo.GetOidcIssuer)
	if err != nil {
		logrus.Fatalf("Failed to resolve the OIDC issuer: %v", err)
	}

	clientID, err := auth.ResolveClientID(ctx, cfg.OIDC.ClientID, contextRepo.GetOidcClientID)
	if err != nil {
		logrus.Fatalf("Failed to resolve the OIDC client id: %v", err)
	}

	insecure, err := contextRepo.GetOidcInsecureSkipVerify(ctx)
	if err != nil {
		logrus.WithError(err).Warn("Could not read oidc.insecureSkipVerify, verifying the issuer certificate")
	}
	if insecure {
		logrus.Warn("oidc.insecureSkipVerify is set: the issuer's certificate is not checked. Trust its CA in the pod instead, anywhere that is not a sandbox.")
	}

	verifier, err := auth.NewVerifier(ctx, auth.Config{
		Issuer:             issuer,
		InsecureSkipVerify: insecure,
		ClientID:           clientID,
	})
	if err != nil {
		logrus.Fatalf("Failed to initialize token verification: %v", err)
	}

	logrus.WithField("issuer", issuer).Info("API token verification enabled")
	return verifier
}

// buildDeployments opens the deployments Git repositories of a topology,
// each cloned under GITOPS_CLONE_DIR/repos (in memory without it), with the
// credentials of GITOPS_CREDENTIALS_DIR.
func buildDeployments(cfg config.GitOpsConfig, topology *gitops.Topology) (*gitops.Deployments, error) {
	open := func(loc gitops.Location) (gitops.Store, error) {
		authMethod, err := gitops.AuthFromDir(loc.URL, cfg.CredentialsDir, cfg.InsecureIgnoreHostKey)
		if err != nil {
			return nil, err
		}
		cloneDir := ""
		if cfg.CloneDir != "" {
			cloneDir = filepath.Join(cfg.CloneDir, "repos", cloneName(loc))
		}
		return gitops.NewGitStore(gitops.GitOptions{
			URL:         loc.URL,
			Branch:      loc.Branch,
			Auth:        authMethod,
			AuthorName:  cfg.AuthorName,
			AuthorEmail: cfg.AuthorEmail,
			CloneDir:    cloneDir,
		})
	}
	d := gitops.NewDeployments(gitops.NewRepositories(topology, open), gitops.NewDefaultFluxRenderer())
	if topology.Forgejo != nil {
		d.Forgejo = gitops.NewForgejoClient(*topology.Forgejo)
	}
	logrus.WithFields(logrus.Fields{
		"platform": topology.Platform.URL + " " + topology.Platform.Branch + " " + topology.Platform.Path,
		"projects": topology.Projects.URL + " " + topology.Projects.Branch + " " + topology.Projects.Path,
		"output":   topology.Output.URL + " " + topology.Output.Branch + " " + topology.Output.Path,
		"engine":   cfg.Engine,
	}).Info("Desired state lives in the deployments repositories")
	return d, nil
}

// cloneName is the directory of a repository's clone: its url and branch,
// made a file name.
func cloneName(loc gitops.Location) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			return r
		}
		return '_'
	}, loc.URL+"_"+loc.Branch)
}
