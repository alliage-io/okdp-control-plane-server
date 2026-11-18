package main

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/okdp/okdp-gitops/catalog"
	"github.com/okdp/okdp-gitops/chartcache"
	"github.com/okdp/okdp-gitops/gitops"

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

// deps is what every stack of the server is built from: the clients of the
// cluster, the deployments repositories, and what is read through them.
type deps struct {
	cfg         *config.Config
	clients     *repository.Clients
	roots       *trust.Roots
	deployments *gitops.Deployments
	charts      *chartcache.Cache
	engine      repository.EngineAdapter
	descriptors repository.DescriptorRepository
	platform    repository.PlatformRepository
	guard       *netguard.Guard
}

// newDeps opens the clients of the cluster and the deployments
// repositories.
func newDeps(cfg *config.Config) (*deps, error) {
	clients, err := repository.NewClients()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Kubernetes clients: %w", err)
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
		return nil, fmt.Errorf("invalid OKDP_CA_FILE: %w", err)
	}
	if err := cfg.GitOps.Validate(); err != nil {
		return nil, fmt.Errorf("invalid GitOps configuration: %w", err)
	}
	topology, err := cfg.GitOps.Topology()
	if err != nil {
		return nil, fmt.Errorf("invalid GitOps topology: %w", err)
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
		return nil, fmt.Errorf("invalid GitOps credentials configuration: %w", err)
	}
	deployments, err := buildDeployments(cfg.GitOps, topology, creds)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare the deployments repositories: %w", err)
	}
	// The packages come from the catalogs catalogs.yaml names, git
	// repositories fetched at their ref, on the host of the platform's
	// repository with its credentials. Every write is checked by a compile
	// of the whole tree with okdp-compiler (okdp-gitops: each claim bound to
	// its provider, of any namespace), against the upstream charts (the
	// chart cache), no chart rendered (service.NewTreeCompiler); the read
	// paths get the same compile (deployments.Tree).
	catalogs := &catalog.Loader{Auth: creds.Catalogs(topology.Platform.URL), CABundle: creds.CABundle()}
	// The puller trusts the platform's CA bundle (OKDP_CA_FILE) beside the
	// system CAs, and reaches a private address only for the hosts of
	// GITOPS_PRIVATE_CHART_HOSTS.
	puller, err := gitops.NewChartPuller(creds, cfg.InsecureOCIRegistries, cfg.PrivateChartHosts)
	if err != nil {
		return nil, fmt.Errorf("GITOPS_PRIVATE_CHART_HOSTS / INSECURE_OCI_REGISTRIES: %w", err)
	}
	chartCache := chartcache.NewCache(cfg.GitOps.ChartCacheDir, puller)
	deployments.Compiler = service.NewTreeCompiler(catalogs, chartCache)

	var engine repository.EngineAdapter
	switch cfg.GitOps.Engine {
	case config.EngineArgoCD:
		engine = repository.NewArgoCDAdapter(clients.Dynamic, cfg.GitOps.ArgoCDNamespace)
	default:
		engine = repository.NewFluxAdapter(clients.Dynamic, cfg.GitOps.ReleasesNamespace)
	}

	// The network the SQL editor and the connection tests reach: never
	// loopback, link-local or the Kubernetes API server, unless
	// SQL_ALLOWED_NETWORKS allows it.
	guard, err := netguard.New(cfg.SQL.AllowedNetworks, netguard.KubernetesAPIServer())
	if err != nil {
		return nil, fmt.Errorf("invalid SQL_ALLOWED_NETWORKS: %w", err)
	}

	return &deps{
		cfg:         cfg,
		clients:     clients,
		roots:       roots,
		deployments: deployments,
		charts:      chartCache,
		engine:      engine,
		// What each instance publishes: its okdp.info.yaml in the output.
		descriptors: repository.NewDescriptorRepository(deployments),
		// Platform configuration: the platform values (platform.yaml, its
		// global.okdp) and the service catalog (catalogs.yaml). Shared by
		// capabilities, catalog and Spark.
		platform: repository.NewPlatformRepository(deployments),
		guard:    guard,
	}, nil
}

// router builds every stack and the routes serving them.
func (d *deps) router(ctx context.Context) (*gin.Engine, error) {
	cfg, deployments, platform, roots, guard := d.cfg, d.deployments, d.platform, d.roots, d.guard
	catalogSource := deployments.Catalogs

	// Projects are the projects/<p>/project.yaml files of the deployments
	// repository; the console also creates (and deletes) the Namespace of
	// the projects it creates.
	projectNamespaces := repository.NewProjectNamespaceRepository(d.clients.Typed)
	projectHandler := handlers.NewProjectHandler(service.NewDefaultProjectService(projectNamespaces, deployments))
	// The bounds of the SQL editor, an authenticated caller's: GET /api/me.
	projectHandler.Sql = models.SqlCapability{
		DefaultRows:             cfg.SQL.DefaultRows,
		MaxRows:                 cfg.SQL.MaxRows,
		MaxResultBytes:          cfg.SQL.MaxResultBytes,
		StatementTimeoutSeconds: int(cfg.SQL.StatementTimeout.Seconds()),
		MaxConcurrentPerUser:    cfg.SQL.MaxConcurrentPerUser,
	}

	// Users and groups in the platform Keycloak realm, through the Admin
	// REST API (KEYCLOAK_*). The realm is located by
	// KEYCLOAK_URL/KEYCLOAK_REALM, else by the issuer the API verifies
	// tokens against.
	identityRepo := repository.NewKeycloakIdentityRepository(cfg, keycloakIssuerSource(cfg, platform), roots.Pool)
	identityHandler := handlers.NewIdentityHandler(service.NewDefaultIdentityService(identityRepo))
	logIdentityBackend(cfg)

	// Capabilities: the platform features derived from the platform values.
	// The identity capability answers on the same condition as the identity
	// routes.
	capabilitiesHandler := handlers.NewCapabilitiesHandler(service.NewDefaultCapabilityService(platform, identityRepo.Available))

	// SecretStores (namespace is dynamic per project). A store of
	// kubernetes auth authenticates as the ServiceAccount of the platform's
	// secret store in the namespace, with its audiences, to its server or
	// one of SECRET_STORE_ALLOWED_SERVERS: the compile of the tree gives
	// them.
	secretStoreRepo := repository.NewSecretStoreRepository(d.clients.Dynamic, d.clients.Discovery)
	secretStoreService := service.NewDefaultSecretStoreService(secretStoreRepo, deployments.Tree, cfg.SecretStores.AllowedServers)
	secretStoreService.SetNetworkGuard(guard)
	secretStoreService.SetRootCAs(roots.Pool)

	// ExternalSecrets (namespace is dynamic per project).
	externalSecretRepo := repository.NewExternalSecretRepository(d.clients.Dynamic, d.clients.Discovery)
	externalSecretService := service.NewDefaultExternalSecretService(externalSecretRepo, secretStoreRepo)

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

	workloads := repository.NewWorkloads(d.clients.Typed, d.clients.Metrics)

	// Services: instances in Git, status from the cluster.
	schemaService := service.NewDefaultPackageSchemaService(platform, catalogSource)
	serviceService := service.NewDefaultServiceService(service.ServiceDeps{
		Deployments:     deployments,
		PlatformRepo:    platform,
		CatalogWriter:   repository.NewCatalogWriterRepository(deployments),
		SchemaService:   schemaService,
		Engine:          d.engine,
		Descriptors:     d.descriptors,
		Workloads:       workloads,
		SidecarPrefixes: cfg.ExcludedSidecarPrefixes,
	})
	serviceHandler := handlers.NewServiceHandler(serviceService, schemaService)
	// What each chart of an instance is rendered with: its compiled values
	// over the defaults of the upstream chart (the chart cache).
	serviceHandler.SetRenderedValuesService(service.NewDefaultRenderedValuesService(deployments, d.charts))

	// Spark (SparkApplication CRUD): every SparkApplication built by the
	// server from an allow-list, its pods run as the ServiceAccount of the
	// project's spark-rbac instance in the compiled tree.
	sparkRepo := repository.NewSparkAppRepository(d.clients.Dynamic)
	sparkService := service.NewDefaultSparkService(sparkRepo, platform, workloads, deployments.Tree)

	// Connections: declared in Git, the project's and the platform's, and
	// the internal ones published by the instances deployed in a project.
	connectionSecrets := repository.NewConnectionSecretRepository(d.clients.Typed)
	connectionService := service.NewDefaultConnectionService(deployments, connectionSecrets, d.descriptors, serviceService.ListServices, contractCatalog)
	connectionService.SetNetworkGuard(guard)
	connectionService.SetRootCAs(roots.Pool)

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
		insecure, err := platform.GetOidcInsecureSkipVerify(ctx)
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
		TokenExchange: buildTokenExchanger(ctx, cfg, platform, roots),
	})

	// The identity block is checked once, at startup.
	if err := checkIdentityConfiguration(ctx, platform); err != nil {
		return nil, err
	}
	warnInsecurePlatformSettings(ctx, platform)

	verifier, err := buildTokenVerifier(ctx, cfg, platform, roots)
	if err != nil {
		return nil, err
	}

	return router.SetupRouter(router.Deps{
		Config:   cfg,
		Verifier: verifier,
		// The platform's authority, which every route's permission is read
		// against (router.Permissions): platform.yaml
		// global.okdp.access.admins and each project.yaml access, as the
		// compiler reads them.
		Authority:       authz.NewAuthority(authz.DeploymentsSource(deployments)),
		Capabilities:    capabilitiesHandler,
		Projects:        projectHandler,
		Identity:        identityHandler,
		SecretStores:    handlers.NewSecretStoreHandler(secretStoreService),
		ExternalSecrets: handlers.NewExternalSecretHandler(externalSecretService),
		Services:        serviceHandler,
		Spark:           handlers.NewSparkHandler(sparkService),
		Connections:     handlers.NewConnectionHandler(connectionService),
		SQL:             handlers.NewSqlHandler(sqlService),
		Claims:          handlers.NewClaimHandler(claimService),
	})
}

// logIdentityBackend says what backs user and group management.
func logIdentityBackend(cfg *config.Config) {
	if cfg.KeycloakClientSecret == "" {
		slog.Info("KEYCLOAK_CLIENT_SECRET is not set: user and group management (/api/v1/identity) is disabled")
		return
	}
	slog.Info("User and group management backed by the Keycloak Admin API (an empty url/realm follows the platform issuer)", "url", cfg.KeycloakURL, "realm", cfg.KeycloakRealm, "clientId", cfg.KeycloakClientID)
}

// checkIdentityConfiguration fails fast on an identity block that cannot
// hold, and stays quiet otherwise. Platform values that cannot be read at
// all are not fatal: they may simply not be there yet on a fresh cluster,
// and the routes that need them already report their own absence.
func checkIdentityConfiguration(ctx context.Context, contextRepo repository.PlatformRepository) error {
	identity, err := contextRepo.GetIdentity(ctx)
	if err != nil {
		// A block that is present and wrong is a configuration error worth
		// stopping for. The two are told apart by the error the repository
		// returns, which only validates what it found.
		if identity != nil {
			return fmt.Errorf("invalid oidc client-provisioning configuration: %w", err)
		}
		slog.Warn("Could not read oidc.clientProvisioning, assuming clients are provisioned elsewhere", "error", err)
		return nil
	}

	provider, err := contextRepo.GetIdentityProvisioningProvider(ctx)
	if err != nil {
		slog.Warn("Could not read identity.provisioning.provider, assuming clients are provisioned elsewhere", "error", err)
		return nil
	}

	switch provider {
	case "", provisioning.ProviderNone:
	case provisioning.ProviderKeycloak:
		// Reported to the console only: the control plane neither creates
		// nor deletes OAuth clients any more.
		slog.Info(fmt.Sprintf("identity.provisioning.provider is %q: the control plane provisions and deletes no OAuth client, the compiled oidc-dcr Jobs (oidc.clientProvisioning: dcr) or the platform (existing) do", provider))
	default:
		return fmt.Errorf("identity.provisioning.provider is %q, which is not supported. Use %q or %q",
			provider, provisioning.ProviderNone, provisioning.ProviderKeycloak)
	}

	slog.Info("Identity configuration accepted", "provisioningProvider", provider)
	return nil
}

// warnInsecurePlatformSettings logs, at startup, the settings of the
// platform values that weaken a connection of the server: still honoured,
// read again on each use.
func warnInsecurePlatformSettings(ctx context.Context, contextRepo repository.PlatformRepository) {
	insecure, err := contextRepo.GetOidcInsecureSkipVerify(ctx)
	if err != nil || !insecure {
		return
	}
	slog.Warn("WARNING: insecure setting global.okdp.oidc.insecureSkipVerify=true (platform.yaml): the certificates of the OIDC issuer, of Keycloak when located from the issuer, and of Trino are not checked; give the server the platform's CA bundle instead (OKDP_CA_FILE, chart tls.caBundleSecret). Never outside a sandbox.")
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

// buildTokenExchanger: the exchange that gives a statement on Trino a
// token of the engine's audience (RFC 8693, auth.NewTokenExchanger), by
// the Keycloak client. Unset, or not usable, none: the caller's token is
// never forwarded, a statement on Trino answers 400 sql-no-token-exchange.
func buildTokenExchanger(ctx context.Context, cfg *config.Config, contextRepo repository.PlatformRepository, roots *trust.Roots) auth.TokenExchanger {
	if cfg.OIDC.Disabled || cfg.SQL.TokenExchangeAudience == "" || cfg.KeycloakClientSecret == "" {
		slog.Warn("No SQL token exchange (SQL_TOKEN_EXCHANGE_AUDIENCE and KEYCLOAK_CLIENT_SECRET): statements on Trino are refused, no caller's token is forwarded")
		return nil
	}
	issuer, err := auth.ResolveIssuer(ctx, cfg.OIDC.Issuer, contextRepo.GetOidcIssuer)
	if err != nil {
		slog.Warn("No SQL token exchange: the OIDC issuer cannot be read", "error", err)
		return nil
	}
	clientID, err := auth.ResolveClientID(ctx, cfg.OIDC.ClientID, contextRepo.GetOidcClientID)
	if err != nil {
		slog.Warn("No SQL token exchange: the OIDC client id cannot be read", "error", err)
		return nil
	}
	insecure, err := contextRepo.GetOidcInsecureSkipVerify(ctx)
	if err != nil {
		slog.Warn("Could not read oidc.insecureSkipVerify, verifying the issuer certificate", "error", err)
	}
	exchanger, err := auth.NewTokenExchanger(ctx, auth.ExchangeConfig{
		Issuer:             issuer,
		InsecureSkipVerify: insecure,
		RootCAs:            roots.Pool,
		ConsoleClientID:    clientID,
		ClientID:           cfg.KeycloakClientID,
		ClientSecret:       cfg.KeycloakClientSecret,
		Audience:           cfg.SQL.TokenExchangeAudience,
	})
	if err != nil {
		slog.Warn("No SQL token exchange: statements on Trino are refused", "error", err)
		return nil
	}
	return exchanger
}

// buildTokenVerifier resolves the issuer the API will trust, or returns nil
// when the deployment asked to run without verification.
func buildTokenVerifier(ctx context.Context, cfg *config.Config, contextRepo repository.PlatformRepository, roots *trust.Roots) (auth.Verifier, error) {
	if cfg.OIDC.Disabled {
		slog.Warn("AUTH_DISABLED is set: the API accepts unauthenticated requests, every caller holding every right. Never do this outside local development.")
		return nil, nil
	}

	issuer, err := auth.ResolveIssuer(ctx, cfg.OIDC.Issuer, contextRepo.GetOidcIssuer)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve the OIDC issuer: %w", err)
	}

	clientID, err := auth.ResolveClientID(ctx, cfg.OIDC.ClientID, contextRepo.GetOidcClientID)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve the OIDC client id: %w", err)
	}

	insecure, err := contextRepo.GetOidcInsecureSkipVerify(ctx)
	if err != nil {
		slog.Warn("Could not read oidc.insecureSkipVerify, verifying the issuer certificate", "error", err)
	}

	verifier, err := auth.NewVerifier(ctx, auth.Config{
		Issuer:             issuer,
		InsecureSkipVerify: insecure,
		RootCAs:            roots.Pool,
		ClientID:           clientID,
		GroupsClaim:        cfg.OIDC.GroupsClaim,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize token verification: %w", err)
	}

	slog.Info("API token verification enabled", "issuer", issuer)
	return verifier, nil
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
	slog.Info("Desired state lives in the deployments repositories", "platform", topology.Platform.URL+" "+topology.Platform.Branch+" "+topology.Platform.Path, "projects", topology.Projects.URL+" "+topology.Projects.Branch+" "+topology.Projects.Path, "output", topology.Output.URL+" "+topology.Output.Branch+" "+topology.Output.Path, "engine", cfg.Engine)
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
