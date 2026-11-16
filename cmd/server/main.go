package main

import (
	"context"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/okdp/okdp-compiler/chartcache"
	"github.com/okdp/okdp-gitops/catalog"
	"github.com/okdp/okdp-gitops/gitops"
	"github.com/sirupsen/logrus"

	"github.com/okdp/okdp-control-plane-server/internal/api/handlers"
	"github.com/okdp/okdp-control-plane-server/internal/api/router"
	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/authz"
	"github.com/okdp/okdp-control-plane-server/internal/buildinfo"
	"github.com/okdp/okdp-control-plane-server/internal/config"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/netguard"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	"github.com/okdp/okdp-control-plane-server/internal/repository/provisioning"
	"github.com/okdp/okdp-control-plane-server/internal/service"
	"github.com/okdp/okdp-control-plane-server/internal/trust"
)

// @title           OKDP Server New API
// @version         1.0
// @description     Minimal API server for OKDP UI New. Every answer carries an X-Request-Id header; a 500 (and a 502 of the SQL routes) answers {"error": <generic message>, "requestId": <id>}, its cause in the server's log under that id. Every route needs a permission, deny by default, read from the platform's own authority: platform admin (a group of platform.yaml global.okdp.access.admins) for the identity routes, the catalog's writes and the creation and deletion of a project; a project role (project.yaml access, okdp-project-<p> an editor by default; a platform admin has every role) for the routes of /api/projects/{name}: viewer for its reads, editor for its writes, admin for its project.yaml. A caller without it answers 403 {"error": <what it needs>, "code": "forbidden"}; GET /api/me tells what the caller may do.
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
	for _, setting := range cfg.InsecureSettings() {
		logrus.Warnf("WARNING: insecure setting %s. Never outside a sandbox.", setting)
	}

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
	// console commits the tree (platform.yaml, catalogs.yaml, projects/)
	// only; the CI of the output repository, running okdp-gitops compile on
	// a schedule, writes the output, in a repository of its own. The
	// commits a user authors name the server in their co-author trailer.
	gitops.Program = buildinfo.Program
	// What every outbound TLS connection trusts: the system's certificates
	// and the platform's CA bundle (OKDP_CA_FILE, chart tls.caBundleSecret).
	roots, err := trust.Load(cfg.CAFile)
	if err != nil {
		logrus.Fatalf("Invalid OKDP_CA_FILE: %v", err)
	}
	if err := cfg.GitOps.Validate(); err != nil {
		logrus.Fatalf("Invalid GitOps configuration: %v", err)
	}
	topology, err := cfg.GitOps.Topology()
	if err != nil {
		logrus.Fatalf("Invalid GitOps topology: %v", err)
	}
	// One Credentials for every credential the server sends: the
	// repositories', the catalogs' and the Forgejo API token, refused over
	// http:// to a host GITOPS_INSECURE_HTTP_HOSTS does not name; their
	// TLS connections trusting the platform's CA bundle too.
	creds, err := gitops.NewCredentials(gitops.CredentialsConfig{
		Dir:                   cfg.GitOps.CredentialsDir,
		InsecureIgnoreHostKey: cfg.GitOps.InsecureIgnoreHostKey,
		InsecureHTTPHosts:     cfg.GitOps.InsecureHTTPHosts,
		CABundle:              roots.PEM,
	})
	if err != nil {
		logrus.Fatalf("Invalid GitOps credentials configuration: %v", err)
	}
	deployments, err := buildDeployments(cfg.GitOps, topology, creds)
	if err != nil {
		logrus.Fatalf("Failed to prepare the deployments repositories: %v", err)
	}
	// The packages come from the catalogs catalogs.yaml names, git
	// repositories fetched at their ref, on the host of the platform's
	// repository with its credentials. Every write is checked by a compile
	// of the whole tree with okdp-compiler (okdp-gitops: each claim bound to
	// its provider, of any namespace), against the upstream charts (the
	// chart cache); the read paths get the same compile (deployments.Tree).
	catalogs := &catalog.Loader{Auth: creds.Catalogs(topology.Platform.URL), CABundle: creds.CABundle()}
	// The puller trusts the platform's CA bundle (OKDP_CA_FILE) beside the
	// system CAs, and reaches a private address only for the hosts of
	// GITOPS_PRIVATE_CHART_HOSTS.
	puller, err := gitops.NewChartPuller(creds, cfg.InsecureOCIRegistries, cfg.PrivateChartHosts)
	if err != nil {
		logrus.Fatalf("GITOPS_PRIVATE_CHART_HOSTS / INSECURE_OCI_REGISTRIES: %v", err)
	}
	chartCache := chartcache.NewCache(cfg.GitOps.ChartCacheDir, puller)
	deployments.Compiler = gitops.NewCompiler(catalogs)
	deployments.Compiler.Charts = chartCache
	catalogSource := deployments.Catalogs
	var engine repository.EngineAdapter
	switch cfg.GitOps.Engine {
	case config.EngineArgoCD:
		engine = repository.NewArgoCDAdapter(k8sClient, cfg.GitOps.ArgoCDNamespace)
	default:
		engine = repository.NewFluxAdapter(k8sClient, cfg.GitOps.ReleasesNamespace)
	}
	// What each instance publishes: its okdp.info.yaml in the output.
	descriptors := repository.NewDescriptorRepository(deployments)

	// Initialize Project stack: projects are the projects/<p>/project.yaml
	// files of the deployments repository; the console also creates (and
	// deletes) the Namespace of the projects it creates.
	projectNamespaces := repository.NewProjectNamespaceRepository(k8sTypedClient)
	projectService := service.NewDefaultProjectService(projectNamespaces, deployments)
	projectHandler := handlers.NewProjectHandler(projectService)

	// Platform configuration: the platform values (platform.yaml, its
	// global.okdp) and the service catalog (catalogs.yaml). Shared by
	// capabilities, catalog and Spark.
	contextRepo := repository.NewPlatformRepository(deployments)
	catalogWriter := repository.NewCatalogWriterRepository(deployments)

	// Initialize Identity stack: users and groups in the platform Keycloak
	// realm, through the Admin REST API (KEYCLOAK_*). The realm is located by
	// KEYCLOAK_URL/KEYCLOAK_REALM, else by the issuer the API verifies tokens
	// against.
	identityRepo := repository.NewKeycloakIdentityRepository(cfg, keycloakIssuerSource(cfg, contextRepo), roots.Pool)
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
	// The bounds of the SQL editor, an authenticated caller's: GET /api/me.
	projectHandler.Sql = models.SqlCapability{
		DefaultRows:             cfg.SQL.DefaultRows,
		MaxRows:                 cfg.SQL.MaxRows,
		MaxResultBytes:          cfg.SQL.MaxResultBytes,
		StatementTimeoutSeconds: int(cfg.SQL.StatementTimeout.Seconds()),
		MaxConcurrentPerUser:    cfg.SQL.MaxConcurrentPerUser,
	}
	capabilitiesHandler := handlers.NewCapabilitiesHandler(capabilityService)

	// Initialize SecretStore stack (namespace is dynamic per project)
	secretStoreRepo := repository.NewSecretStoreRepository(k8sClient, k8sDiscoveryClient)
	// A store of kubernetes auth authenticates as the ServiceAccount of the
	// platform's secret store in the namespace, with its audiences, to its
	// server or one of SECRET_STORE_ALLOWED_SERVERS: the compile of the
	// tree gives them.
	secretStoreService := service.NewDefaultSecretStoreService(secretStoreRepo, deployments.Tree, cfg.SecretStores.AllowedServers)
	secretStoreHandler := handlers.NewSecretStoreHandler(secretStoreService)

	// Initialize ExternalSecret stack (namespace is dynamic per project)
	externalSecretRepo := repository.NewExternalSecretRepository(k8sClient, k8sDiscoveryClient)
	externalSecretService := service.NewDefaultExternalSecretService(externalSecretRepo, secretStoreRepo)
	externalSecretHandler := handlers.NewExternalSecretHandler(externalSecretService)

	// The contracts are the catalogs' (contracts/ of each repository): the
	// connection forms, and what a claim may request (x-okdp-claim).
	contractCatalog := service.NewCatalogContracts(func(ctx context.Context) (fs.FS, error) {
		cats, err := catalogSource(ctx)
		if err != nil {
			return nil, err
		}
		index, err := cats.Index()
		if err != nil {
			return nil, err
		}
		return index.Contracts(), nil
	})
	// Claims: the named claims of the projects, the classes a claim may be
	// of, the claims of an instance, bound by the compile of the tree.
	claimService := service.NewDefaultClaimService(deployments, contractCatalog)
	claimHandler := handlers.NewClaimHandler(claimService)

	// Initialize Service stack (instances in Git, status from the cluster)
	schemaService := service.NewDefaultPackageSchemaService(contextRepo, catalogSource)
	serviceService := service.NewDefaultServiceService(service.ServiceDeps{
		Deployments:     deployments,
		PlatformRepo:    contextRepo,
		CatalogWriter:   catalogWriter,
		SchemaService:   schemaService,
		Engine:          engine,
		Descriptors:     descriptors,
		K8sClient:       k8sClient,
		TypedClient:     k8sTypedClient,
		SidecarPrefixes: cfg.ExcludedSidecarPrefixes,
	})
	serviceHandler := handlers.NewServiceHandler(serviceService, schemaService)
	// What each chart of an instance is rendered with: its compiled values
	// over the defaults of the upstream chart (the chart cache).
	serviceHandler.SetRenderedValuesService(service.NewDefaultRenderedValuesService(deployments, chartCache))

	// Initialize Spark stack (SparkApplication CRUD): every SparkApplication
	// built by the server from an allow-list, its pods run as the
	// ServiceAccount of the project's spark-rbac instance in the compiled
	// tree.
	sparkRepo := repository.NewSparkAppRepository(k8sClient)
	sparkService := service.NewDefaultSparkService(sparkRepo, contextRepo, k8sTypedClient, deployments.Tree)
	sparkHandler := handlers.NewSparkHandler(sparkService)

	// Initialize Connection stack (connections declared in Git, the
	// project's and the platform's, + internal ones published by the
	// instances deployed in a project)
	connectionSecrets := repository.NewConnectionSecretRepository(k8sTypedClient)
	// The network the SQL editor and the connection tests reach: never
	// loopback, link-local or the Kubernetes API server, unless
	// SQL_ALLOWED_NETWORKS allows it.
	guard, err := netguard.New(cfg.SQL.AllowedNetworks, netguard.KubernetesAPIServer())
	if err != nil {
		logrus.Fatalf("Invalid SQL_ALLOWED_NETWORKS: %v", err)
	}
	connectionService := service.NewDefaultConnectionService(deployments, connectionSecrets, descriptors, serviceService.ListServices, contractCatalog)
	connectionService.SetNetworkGuard(guard)
	connectionService.SetRootCAs(roots.Pool)
	secretStoreService.SetNetworkGuard(guard)
	secretStoreService.SetRootCAs(roots.Pool)
	connectionHandler := handlers.NewConnectionHandler(connectionService)

	// SQL editor: statements run on a deployed engine (Trino) at the URL of its
	// instance descriptor with the caller's bearer token, or on a PostgreSQL
	// connection declared in the tree (the project's, the platform's, a named
	// claim's) with the credentials of an allowed Secret. Certificates are
	// verified against the system's CAs and the platform's bundle
	// (OKDP_CA_FILE); Trino's are taken on trust when the platform does so
	// for its issuer (discouraged).
	sqlConnections := service.SqlConnectionSource{
		Get:     deployments.GetConnection,
		Claims:  deployments.ListClaims,
		Tree:    deployments.Tree,
		Secrets: connectionSecrets,
		ExternalSecrets: func(ctx context.Context, namespace string) ([]crd.ESOExternalSecret, error) {
			if !externalSecretRepo.Available(ctx) {
				return nil, nil
			}
			return externalSecretRepo.List(ctx, namespace)
		},
	}
	sqlService := service.NewDefaultSqlService(serviceService.GetService, sqlConnections, func(ctx context.Context) bool {
		insecure, err := contextRepo.GetOidcInsecureSkipVerify(ctx)
		return err == nil && insecure
	}, service.SqlOptions{
		Limits: service.SqlLimits{
			StatementTimeout:     cfg.SQL.StatementTimeout,
			DefaultRows:          cfg.SQL.DefaultRows,
			MaxRows:              cfg.SQL.MaxRows,
			MaxResultBytes:       cfg.SQL.MaxResultBytes,
			MaxConcurrentPerUser: cfg.SQL.MaxConcurrentPerUser,
		},
		Guard:   guard,
		RootCAs: roots.Pool,
		// Write mode is a project editor's, as every write of a project.
		AuthorizeWrite: func(ctx context.Context, project string) error {
			return authz.Require(ctx, authz.ProjectEditor, project)
		},
	})
	sqlHandler := handlers.NewSqlHandler(sqlService)

	// The identity block is checked once, at startup.
	checkIdentityConfiguration(context.Background(), contextRepo)
	warnInsecurePlatformSettings(context.Background(), contextRepo)

	verifier := buildTokenVerifier(context.Background(), cfg, contextRepo, roots)

	// Release mode unless asked otherwise: debug mode prints every route and
	// warns on each start, which is noise in a pod. GIN_MODE=debug brings it back.
	if os.Getenv(gin.EnvGinMode) == "" {
		gin.SetMode(gin.ReleaseMode)
	}

	// The platform's authority, which every route's permission is read
	// against (router.Permissions): platform.yaml global.okdp.access.admins
	// and each project.yaml access, as the compiler reads them.
	authority := authz.NewAuthority(authz.DeploymentsSource(deployments))

	r := router.SetupRouter(cfg, verifier, authority, capabilitiesHandler, projectHandler, identityHandler, secretStoreHandler, externalSecretHandler, serviceHandler, sparkHandler, connectionHandler, sqlHandler, claimHandler)

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

// checkIdentityConfiguration fails fast on an identity block that cannot
// hold, and stays quiet otherwise. Platform values that cannot be read at
// all are not fatal: they may simply not be there yet on a fresh cluster,
// and the routes that need them already report their own absence.
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

	provider, err := contextRepo.GetIdentityProvisioningProvider(ctx)
	if err != nil {
		logrus.WithError(err).Warn("Could not read identity.provisioning.provider, assuming clients are provisioned elsewhere")
		return
	}

	switch provider {
	case "", provisioning.ProviderNone:
	case provisioning.ProviderKeycloak:
		// Reported to the console only: the control plane neither creates
		// nor deletes OAuth clients any more.
		logrus.Infof("identity.provisioning.provider is %q: the control plane provisions and deletes no OAuth client, the compiled oidc-dcr Jobs (oidc.clientProvisioning: dcr) or the platform (existing) do", provider)
	default:
		logrus.Fatalf("identity.provisioning.provider is %q, which is not supported. Use %q or %q.",
			provider, provisioning.ProviderNone, provisioning.ProviderKeycloak)
	}

	logrus.WithField("provisioningProvider", provider).Info("Identity configuration accepted")
}

// warnInsecurePlatformSettings logs, at startup, the settings of the
// platform values that weaken a connection of the server: still honoured,
// read again on each use.
func warnInsecurePlatformSettings(ctx context.Context, contextRepo repository.PlatformRepository) {
	insecure, err := contextRepo.GetOidcInsecureSkipVerify(ctx)
	if err != nil || !insecure {
		return
	}
	logrus.Warn("WARNING: insecure setting global.okdp.oidc.insecureSkipVerify=true (platform.yaml): the certificates of the OIDC issuer, of Keycloak when located from the issuer, and of Trino are not checked; give the server the platform's CA bundle instead (OKDP_CA_FILE, chart tls.caBundleSecret). Never outside a sandbox.")
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
func buildTokenVerifier(ctx context.Context, cfg *config.Config, contextRepo repository.PlatformRepository, roots *trust.Roots) auth.Verifier {
	if cfg.OIDC.Disabled {
		logrus.Warn("AUTH_DISABLED is set: the API accepts unauthenticated requests, every caller holding every right. Never do this outside local development.")
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

	verifier, err := auth.NewVerifier(ctx, auth.Config{
		Issuer:             issuer,
		InsecureSkipVerify: insecure,
		RootCAs:            roots.Pool,
		ClientID:           clientID,
		GroupsClaim:        cfg.OIDC.GroupsClaim,
	})
	if err != nil {
		logrus.Fatalf("Failed to initialize token verification: %v", err)
	}

	logrus.WithField("issuer", issuer).Info("API token verification enabled")
	return verifier
}

// buildDeployments opens the deployments Git repositories of a topology,
// each cloned under GITOPS_CLONE_DIR/repos (in memory without it), with the
// credentials creds (GITOPS_CREDENTIALS_DIR).
func buildDeployments(cfg config.GitOpsConfig, topology *gitops.Topology, creds *gitops.Credentials) (*gitops.Deployments, error) {
	open := func(loc gitops.Location) (gitops.Store, error) {
		authMethod, err := creds.For(loc.URL)
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
			CABundle:    creds.CABundle(),
		})
	}
	d := gitops.NewDeployments(gitops.NewRepositories(topology, open))
	if topology.Forgejo != nil {
		d.Forgejo = gitops.NewForgejoClient(*topology.Forgejo, creds)
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
