package router

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/okdp/okdp-control-plane-server/internal/api/handlers"
	"github.com/okdp/okdp-control-plane-server/internal/api/middleware"
	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/authz"
	"github.com/okdp/okdp-control-plane-server/internal/config"
)

// Short names of the permissions of the table.
const (
	public        = authz.Public
	authenticated = authz.Authenticated
	platformAdmin = authz.PlatformAdmin
	viewer        = authz.ProjectViewer
	editor        = authz.ProjectEditor
	projectAdmin  = authz.ProjectAdmin
)

// route is a key of the table.
func route(method, path string) string { return middleware.RouteKey(method, path) }

// Permissions is what each route of the API needs, deny by default: a
// route missing here answers 403 to everyone (middleware.Authorize). It
// mirrors the platform's authority (decision D-1), the one Argo CD, the
// namespaces' RoleBindings and Forgejo enforce without the control plane:
//
//   - platform admin, a group of platform.yaml global.okdp.access.admins:
//     users and groups (/v1/identity), the catalog's menu
//     (/platform-services writes), the creation and the deletion of a
//     project; a platform admin has every project role;
//   - project viewer, editor, admin, a group's role in project.yaml access
//     (okdp-project-<p> an editor by default): the reads of a project are
//     a viewer's, its writes an editor's, its project.yaml an admin's;
//   - authenticated: what the platform shows every user, its catalog and
//     its settings; the lists of projects show the caller's only.
//
// A SQL statement on a connection runs with credentials of the project's
// Secrets, which a viewer cannot read (the namespace's view role): an
// editor's. On a Trino service it runs with the caller's own token, the
// engine authorizing it: a viewer's. Write mode is an editor's
// (SqlOptions.AuthorizeWrite).
var Permissions = middleware.Permissions{
	route(http.MethodGet, "/api/capabilities"): public,
	route(http.MethodGet, "/api/me"):           authenticated,

	route(http.MethodGet, "/api/projects"):               authenticated,
	route(http.MethodGet, "/api/projects/stream"):        authenticated,
	route(http.MethodPost, "/api/projects"):              platformAdmin,
	route(http.MethodGet, "/api/projects/:name"):         viewer,
	route(http.MethodPut, "/api/projects/:name"):         projectAdmin,
	route(http.MethodDelete, "/api/projects/:name"):      platformAdmin,
	route(http.MethodGet, "/api/projects/:name/metrics"): viewer,

	route(http.MethodGet, "/api/v1/identity/users"):           platformAdmin,
	route(http.MethodGet, "/api/v1/identity/users/:name"):     platformAdmin,
	route(http.MethodPost, "/api/v1/identity/users"):          platformAdmin,
	route(http.MethodPut, "/api/v1/identity/users/:name"):     platformAdmin,
	route(http.MethodDelete, "/api/v1/identity/users/:name"):  platformAdmin,
	route(http.MethodGet, "/api/v1/identity/groups"):          platformAdmin,
	route(http.MethodPost, "/api/v1/identity/groups"):         platformAdmin,
	route(http.MethodPut, "/api/v1/identity/groups/:name"):    platformAdmin,
	route(http.MethodDelete, "/api/v1/identity/groups/:name"): platformAdmin,

	route(http.MethodGet, "/api/projects/:name/secret-stores"):                   viewer,
	route(http.MethodPost, "/api/projects/:name/secret-stores"):                  editor,
	route(http.MethodPost, "/api/projects/:name/secret-stores/test"):             editor,
	route(http.MethodPut, "/api/projects/:name/secret-stores/:storeName"):        editor,
	route(http.MethodDelete, "/api/projects/:name/secret-stores/:storeName"):     editor,
	route(http.MethodGet, "/api/projects/:name/secret-stores/:storeName/status"): viewer,
	route(http.MethodGet, "/api/projects/:name/external-secrets"):                viewer,
	route(http.MethodPost, "/api/projects/:name/external-secrets"):               editor,
	route(http.MethodPut, "/api/projects/:name/external-secrets/:esName"):        editor,
	route(http.MethodDelete, "/api/projects/:name/external-secrets/:esName"):     editor,
	route(http.MethodGet, "/api/projects/:name/external-secrets/:esName/status"): viewer,

	route(http.MethodGet, "/api/contracts"):                                      authenticated,
	route(http.MethodGet, "/api/projects/:name/connections"):                     viewer,
	route(http.MethodGet, "/api/projects/:name/connections/internal"):            viewer,
	route(http.MethodGet, "/api/projects/:name/connections/selectable"):          viewer,
	route(http.MethodGet, "/api/projects/:name/connections/:connName/consumers"): viewer,
	route(http.MethodPost, "/api/projects/:name/connections/:connName/sql"):      editor,
	route(http.MethodPost, "/api/projects/:name/connections"):                    editor,
	route(http.MethodPost, "/api/projects/:name/connections/test"):               editor,
	route(http.MethodPut, "/api/projects/:name/connections/:connName"):           editor,
	route(http.MethodDelete, "/api/projects/:name/connections/:connName"):        editor,

	route(http.MethodGet, "/api/platform-services"):                       authenticated,
	route(http.MethodGet, "/api/platform-categories"):                     authenticated,
	route(http.MethodPost, "/api/platform-services"):                      platformAdmin,
	route(http.MethodPut, "/api/platform-services/:serviceName"):          platformAdmin,
	route(http.MethodDelete, "/api/platform-services/:serviceName"):       platformAdmin,
	route(http.MethodGet, "/api/platform-services/:serviceName/versions"): authenticated,
	route(http.MethodGet, "/api/platform-services/:serviceName/schema"):   authenticated,
	route(http.MethodGet, "/api/platform-services/:serviceName/inputs"):   authenticated,
	route(http.MethodGet, "/api/profile-images"):                          authenticated,

	route(http.MethodGet, "/api/projects/:name/services"):                                 viewer,
	route(http.MethodGet, "/api/projects/:name/services/stream"):                          viewer,
	route(http.MethodPost, "/api/projects/:name/services"):                                editor,
	route(http.MethodGet, "/api/projects/:name/services/:serviceName"):                    viewer,
	route(http.MethodPatch, "/api/projects/:name/services/:serviceName/parameters"):       editor,
	route(http.MethodDelete, "/api/projects/:name/services/:serviceName"):                 editor,
	route(http.MethodGet, "/api/projects/:name/services/:serviceName/pods"):               viewer,
	route(http.MethodGet, "/api/projects/:name/services/:serviceName/pods/:podName/logs"): viewer,
	route(http.MethodGet, "/api/projects/:name/services/:serviceName/metrics"):            viewer,
	route(http.MethodGet, "/api/projects/:name/services/:serviceName/values"):             viewer,
	route(http.MethodPost, "/api/projects/:name/services/:serviceName/sql"):               viewer,
	route(http.MethodGet, "/api/projects/:name/services/:serviceName/claims"):             viewer,

	route(http.MethodGet, "/api/projects/:name/claims"):               viewer,
	route(http.MethodPost, "/api/projects/:name/claims"):              editor,
	route(http.MethodPut, "/api/projects/:name/claims/:claimName"):    editor,
	route(http.MethodDelete, "/api/projects/:name/claims/:claimName"): editor,
	route(http.MethodGet, "/api/projects/:name/provisioning"):         viewer,

	route(http.MethodGet, "/api/spark-config"):                            authenticated,
	route(http.MethodGet, "/api/spark-app-schema"):                        authenticated,
	route(http.MethodGet, "/api/projects/:name/spark-apps"):               viewer,
	route(http.MethodGet, "/api/projects/:name/spark-apps/stream"):        viewer,
	route(http.MethodPost, "/api/projects/:name/spark-apps"):              editor,
	route(http.MethodPost, "/api/projects/:name/spark-apps/yaml"):         editor,
	route(http.MethodGet, "/api/projects/:name/spark-apps/:appName"):      viewer,
	route(http.MethodPut, "/api/projects/:name/spark-apps/:appName"):      editor,
	route(http.MethodDelete, "/api/projects/:name/spark-apps/:appName"):   editor,
	route(http.MethodGet, "/api/projects/:name/spark-apps/:appName/logs"): viewer,
	route(http.MethodGet, "/api/projects/:name/spark-apps/:appName/ui"):   viewer,
}

// Deps is what the router serves: the configuration, the token verifier
// and the platform's authority, and one handler per stack.
type Deps struct {
	Config *config.Config
	// Verifier checks the bearer tokens; nil serves the API without
	// verification, which only AUTH_DISABLED produces, every caller holding
	// every right.
	Verifier auth.Verifier
	// Authority is the platform's authority each route's permission is read
	// against (Permissions).
	Authority *authz.Authority

	Capabilities    *handlers.CapabilitiesHandler
	Projects        *handlers.ProjectHandler
	Identity        *handlers.IdentityHandler
	SecretStores    *handlers.SecretStoreHandler
	ExternalSecrets *handlers.ExternalSecretHandler
	Services        *handlers.ServiceHandler
	Spark           *handlers.SparkHandler
	Connections     *handlers.ConnectionHandler
	SQL             *handlers.SqlHandler
	Claims          *handlers.ClaimHandler
}

// SetupRouter initializes the Gin router and defines routes. It fails on
// a TRUSTED_PROXIES entry that is no address or CIDR.
func SetupRouter(d Deps) (*gin.Engine, error) {
	cfg, verifier, authority := d.Config, d.Verifier, d.Authority
	capabilitiesHandler, projectHandler, identityHandler := d.Capabilities, d.Projects, d.Identity
	secretStoreHandler, externalSecretHandler := d.SecretStores, d.ExternalSecrets
	serviceHandler, sparkHandler, connectionHandler := d.Services, d.Spark, d.Connections
	sqlHandler, claimHandler := d.SQL, d.Claims

	r := gin.New() // Use New() to skip default logger/recovery (we add them manually)

	// The audit log's ip, and whatever else reads c.ClientIP(), believes
	// X-Forwarded-For from these peers only: none by default, the client
	// is then the address of the connection (SRV-27).
	if err := r.SetTrustedProxies(cfg.TrustedProxies); err != nil {
		return nil, fmt.Errorf("TRUSTED_PROXIES: %w", err)
	}

	// Middleware: the request id first, for the log line and the error
	// answers.
	r.Use(middleware.RequestID())
	r.Use(middleware.RequestLogger())
	r.Use(gin.Recovery())
	r.Use(middleware.SecurityHeaders())
	r.Use(corsMiddleware(cfg))
	r.Use(middleware.LimitRequestBody(middleware.DefaultMaxBodyBytes))

	// No /swagger route: the API's description is docs/swagger.yaml of the
	// repository (make swagger), not served.

	// Health check
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	// API Routes
	api := r.Group("/api")
	if verifier != nil {
		// On the group, not per route: what is added below is authenticated
		// unless the middleware lists it as public.
		api.Use(middleware.RequireAuthentication(verifier))
	}
	// After authentication, before any group middleware: an object name that
	// could leave its directory never reaches a store (RequireProject
	// included).
	api.Use(middleware.ValidatePathNames())
	// Deny by default: each route the permission of Permissions, before
	// any group middleware (RequireProject, RequireAPI) or handler.
	api.Use(middleware.Authorize(Permissions, authority, verifier == nil))
	{
		// Platform capabilities (UI feature discovery)
		api.GET("/capabilities", capabilitiesHandler.GetCapabilities)
		// What the caller may do: platform admin, its role in each project.
		api.GET("/me", projectHandler.GetCallerAccess)

		// Projects (projects/<p>/project.yaml in the deployments repository)
		api.GET("/projects", projectHandler.ListProjects)
		api.GET("/projects/stream", projectHandler.StreamProjects)
		api.POST("/projects", projectHandler.CreateProject)
		api.GET("/projects/:name", projectHandler.GetProject)
		api.PUT("/projects/:name", projectHandler.UpdateProject)
		api.DELETE("/projects/:name", projectHandler.DeleteProject)

		// Identity: users and groups in the platform Keycloak realm, through
		// the Admin REST API. Guarded once here rather than in each handler, so
		// a route added later cannot slip through unguarded: without Keycloak
		// admin credentials the whole group answers 501.
		identity := api.Group("/v1/identity", identityHandler.RequireAPI())
		{
			// Users
			identity.GET("/users", identityHandler.ListUsers)
			identity.GET("/users/:name", identityHandler.GetUser)
			identity.POST("/users", identityHandler.CreateUser)
			identity.PUT("/users/:name", identityHandler.UpdateUser)
			identity.DELETE("/users/:name", identityHandler.DeleteUser)

			// Groups
			identity.GET("/groups", identityHandler.ListGroups)
			identity.POST("/groups", identityHandler.CreateGroup)
			identity.PUT("/groups/:name", identityHandler.UpdateGroup)
			identity.DELETE("/groups/:name", identityHandler.DeleteGroup)
		}

		// Secret Stores (scoped per project namespace)
		requireProject := middleware.RequireProject(projectHandler.Resolve)

		// Both groups rest on the external-secrets CRDs, absent from a cluster
		// with no vault integration. Each group is gated on the CRD it actually
		// uses: a rolling install can serve one and not the other, and the
		// answer must be the 501 rather than a failure deeper in.
		esoUnavailable := "Vault integration is not available on this cluster: the external-secrets CRDs are not installed."
		requireSecretStores := handlers.RequireFeature(
			func(c *gin.Context) bool { return secretStoreHandler.Available(c.Request.Context()) },
			"external-secrets",
			esoUnavailable,
		)
		requireExternalSecrets := handlers.RequireFeature(
			func(c *gin.Context) bool { return externalSecretHandler.Available(c.Request.Context()) },
			"external-secrets",
			esoUnavailable,
		)

		secretStores := api.Group("/projects/:name/secret-stores", requireProject, requireSecretStores)
		{
			secretStores.GET("", secretStoreHandler.ListSecretStores)
			secretStores.POST("", secretStoreHandler.CreateSecretStore)
			secretStores.POST("/test", secretStoreHandler.TestConnection)
			secretStores.PUT("/:storeName", secretStoreHandler.UpdateSecretStore)
			secretStores.DELETE("/:storeName", secretStoreHandler.DeleteSecretStore)
			secretStores.GET("/:storeName/status", secretStoreHandler.GetStatus)
		}

		// External Secrets (scoped per project namespace)
		externalSecrets := api.Group("/projects/:name/external-secrets", requireProject, requireExternalSecrets)
		{
			externalSecrets.GET("", externalSecretHandler.ListExternalSecrets)
			externalSecrets.POST("", externalSecretHandler.CreateExternalSecret)
			externalSecrets.PUT("/:esName", externalSecretHandler.UpdateExternalSecret)
			externalSecrets.DELETE("/:esName", externalSecretHandler.DeleteExternalSecret)
			externalSecrets.GET("/:esName/status", externalSecretHandler.GetExternalSecretStatus)
		}

		// Contracts available for creation, and whether connections can be
		// stored (a deployments repository is configured).
		api.GET("/contracts", connectionHandler.GetContracts)

		// Connections (scoped per project namespace). "internal" lists what the
		// project's deployed services expose. The rest are user-declared.
		connections := api.Group("/projects/:name/connections", requireProject)
		{
			connections.GET("", connectionHandler.ListConnections)
			connections.GET("/internal", connectionHandler.ListInternalConnections)
			connections.GET("/selectable", connectionHandler.ListSelectable)
			connections.GET("/:connName/consumers", connectionHandler.ListConsumers)
			connections.POST("/:connName/sql", sqlHandler.ExecuteOnConnection)
			connections.POST("", connectionHandler.CreateConnection)
			connections.POST("/test", connectionHandler.TestConnection)
			connections.PUT("/:connName", connectionHandler.UpdateConnection)
			connections.DELETE("/:connName", connectionHandler.DeleteConnection)
		}

		// Platform services (managed OKDP services)
		api.GET("/platform-services", serviceHandler.GetPlatformServices)
		api.GET("/platform-categories", serviceHandler.GetMenuCategories)
		api.POST("/platform-services", serviceHandler.CreatePlatformService)
		api.PUT("/platform-services/:serviceName", serviceHandler.UpdatePlatformService)
		api.DELETE("/platform-services/:serviceName", serviceHandler.DeletePlatformService)
		api.GET("/platform-services/:serviceName/versions", serviceHandler.GetServiceVersions)
		api.GET("/platform-services/:serviceName/schema", serviceHandler.GetServiceSchema)
		api.GET("/platform-services/:serviceName/inputs", serviceHandler.GetServiceInputs)
		api.GET("/profile-images", serviceHandler.GetProfileImages)

		// Deployed services per project (instances in the deployments repository)
		services := api.Group("/projects/:name/services", requireProject)
		{
			services.GET("", serviceHandler.ListServices)
			services.GET("/stream", serviceHandler.StreamServices)
			services.POST("", serviceHandler.DeployService)
			services.GET("/:serviceName", serviceHandler.GetService)
			services.PATCH("/:serviceName/parameters", serviceHandler.UpdateServiceParameters)
			services.DELETE("/:serviceName", serviceHandler.DeleteService)
			services.GET("/:serviceName/pods", serviceHandler.ListPods)
			services.GET("/:serviceName/pods/:podName/logs", serviceHandler.GetPodLogs)
			services.GET("/:serviceName/metrics", serviceHandler.GetServiceMetrics)
			services.GET("/:serviceName/values", serviceHandler.GetRenderedValues)
			services.POST("/:serviceName/sql", sqlHandler.ExecuteQuery)
		}
		api.GET("/projects/:name/metrics", requireProject, serviceHandler.GetProjectMetrics)

		// Claims: the named claims of a project (its project.yaml), the
		// classes a claim may be of, and the claims of an instance, bound to
		// their providers.
		claims := api.Group("/projects/:name/claims", requireProject)
		{
			claims.GET("", claimHandler.ListNamedClaims)
			claims.POST("", claimHandler.CreateNamedClaim)
			claims.PUT("/:claimName", claimHandler.UpdateNamedClaim)
			claims.DELETE("/:claimName", claimHandler.DeleteNamedClaim)
		}
		api.GET("/projects/:name/provisioning", requireProject, claimHandler.GetProvisioning)
		api.GET("/projects/:name/services/:serviceName/claims", requireProject, claimHandler.GetInstanceClaims)

		// Spark config (from the platform values) + CRD schema
		api.GET("/spark-config", sparkHandler.GetSparkConfig)
		api.GET("/spark-app-schema", sparkHandler.GetSparkAppSchema)

		// SparkApplications per project
		sparkApps := api.Group("/projects/:name/spark-apps", requireProject)
		{
			sparkApps.GET("", sparkHandler.ListSparkApps)
			sparkApps.GET("/stream", sparkHandler.StreamSparkApps)
			sparkApps.POST("", sparkHandler.SubmitSparkApp)
			sparkApps.POST("/yaml", sparkHandler.SubmitSparkAppYAML)
			sparkApps.GET("/:appName", sparkHandler.GetSparkApp)
			sparkApps.PUT("/:appName", sparkHandler.UpdateSparkApp)
			sparkApps.DELETE("/:appName", sparkHandler.DeleteSparkApp)
			sparkApps.GET("/:appName/logs", sparkHandler.GetSparkAppLogs)
			sparkApps.GET("/:appName/ui", sparkHandler.GetSparkUI)
		}
	}

	return r, nil
}

func corsMiddleware(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", cfg.AllowedOrigins)
		c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, accept, origin, Cache-Control, X-Requested-With, X-Request-Id")
		c.Writer.Header().Set("Access-Control-Expose-Headers", "X-Request-Id")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS, GET, PUT, DELETE, PATCH")
		// Tells caches the CORS answer is about the requesting origin, so one
		// origin's cached response is never replayed to another.
		c.Writer.Header().Add("Vary", "Origin")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}
