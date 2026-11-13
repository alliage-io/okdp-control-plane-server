package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/okdp/okdp-gitops/gitops"
)

// Config holds the application configuration
type Config struct {
	ServerPort              string
	PlatformNamespace       string
	AllowedOrigins          string
	LogLevel                string
	ExcludedSidecarPrefixes []string
	// InsecureOCIRegistries lists registry hosts reached over plain HTTP
	// (the upstream charts of the chart cache): a development-sandbox
	// affordance for local registries without TLS.
	InsecureOCIRegistries []string
	// OIDC configures the verification of the console's bearer token.
	OIDC OIDCConfig
	// GitOps configures the deployments repositories, the only desired-state
	// store, and the engine reconciling it.
	GitOps GitOpsConfig
	// SQL bounds the SQL editor and the network the connection tests reach.
	SQL SQLConfig
	// SecretStores bounds the SecretStores of the projects.
	SecretStores SecretStoresConfig

	// Keycloak Admin REST API, backing user and group management
	// (/api/v1/identity). The client is a confidential service account of the
	// realm holding the roles view-users, query-users, manage-users and
	// query-groups of the realm's management client (realm-management, or
	// master-realm when the realm is master). User management is off while
	// KeycloakClientSecret is empty.
	//
	// KeycloakURL (base URL, without /realms/...) and KeycloakRealm override
	// the platform issuer; empty, both are read from the issuer the API
	// verifies tokens against (OIDC_ISSUER, else the platform values), which
	// has the form <url>/realms/<realm>.
	KeycloakURL          string
	KeycloakRealm        string
	KeycloakClientID     string
	KeycloakClientSecret string
	// KeycloakTLSInsecure skips the certificate check of the Keycloak
	// endpoint (sandboxes). global.okdp.oidc.insecureSkipVerify has the same
	// effect when the endpoint comes from the platform issuer.
	KeycloakTLSInsecure bool
}

// Engines reconciling the deployments repository.
const (
	EngineFlux   = "flux"
	EngineArgoCD = "argocd"
)

type GitOpsConfig struct {
	// ConfigFile is the topology file (GITOPS_CONFIG), required: where the
	// platform, the projects and the output live, the output in a
	// repository of its own.
	ConfigFile string
	// RemovedRepoURL is GITOPS_REPO_URL, the repository of the removed
	// single-repository mode: refused when set, for a deployment that still
	// names it not to start on another store than it thinks.
	RemovedRepoURL string
	// CredentialsDir holds the mounted credentials Secret (Flux GitRepository
	// keys: username/password, bearerToken, or identity/known_hosts).
	CredentialsDir string
	// InsecureIgnoreHostKey accepts any SSH host key. Sandboxes only.
	InsecureIgnoreHostKey bool
	// InsecureHTTPHosts are the hosts a credential (the repositories', the
	// catalogs', the Forgejo token) is sent to over http://, in clear
	// (GITOPS_INSECURE_HTTP_HOSTS, comma separated host names or globs of
	// them, path.Match): none by default, a credential for an http:// URL
	// is refused. Name the exact hosts (a sandbox's in-cluster Forgejo).
	InsecureHTTPHosts []string
	AuthorName        string
	AuthorEmail       string
	// CloneDir holds the local clones, a cache (an emptyDir), one directory
	// per repository under repos/; empty keeps them in memory.
	CloneDir string
	// ChartCacheDir is the chart cache (GITOPS_CHART_CACHE_DIR): the
	// upstream charts of the compiled instances, for the checks of a compile
	// and the chart defaults. <CloneDir>/charts by default, else under the
	// temporary directory.
	ChartCacheDir string
	// Engine is flux or argocd: which objects carry the render/sync status.
	Engine string
	// ReleasesNamespace holds the Flux HelmReleases and their values
	// ConfigMaps.
	ReleasesNamespace string
	// ArgoCDNamespace holds the Argo CD Applications.
	ArgoCDNamespace string
}

// Validate refuses a configuration the server cannot run with.
func (g GitOpsConfig) Validate() error {
	if g.RemovedRepoURL != "" {
		return errors.New("GITOPS_REPO_URL is no longer read: the single-repository mode is gone, the output is in a repository of its own, never one of the sources; name the repositories of the platform, the projects and the output in the topology file GITOPS_CONFIG, and remove GITOPS_REPO_URL, GITOPS_BRANCH and GITOPS_PATH")
	}
	if g.ConfigFile == "" {
		return errors.New("GITOPS_CONFIG is required: the topology file naming the repositories of the platform, the projects and the output, the only desired-state store")
	}
	if g.Engine != EngineFlux && g.Engine != EngineArgoCD {
		return errors.New("GITOPS_ENGINE must be flux or argocd")
	}
	return nil
}

// Topology returns where the parts of the tree live: the GITOPS_CONFIG
// file.
func (g GitOpsConfig) Topology() (*gitops.Topology, error) {
	return gitops.LoadTopology(g.ConfigFile)
}

type OIDCConfig struct {
	// It overrides the issuer the platform values declare. Empty is the
	// normal case: the platform values are where that setting lives.
	Issuer string
	// ClientID overrides the client id the platform values declare.
	ClientID string
	// GroupsClaim is the claim of the token that holds the caller's
	// groups (OIDC_GROUPS_CLAIM, groups by default): the groups of
	// platform.yaml global.okdp.access.admins and of each project.yaml
	// access, by name (Keycloak's groups mapper, "Full group path" off).
	GroupsClaim string
	// Disabled leaves the API open to anyone who can reach the port.
	Disabled bool
}

// SQLConfig bounds the SQL editor (POST .../connections/:c/sql and
// .../services/:s/sql) and the connections it, and the connection tests,
// open.
type SQLConfig struct {
	// StatementTimeout bounds one execution (SQL_STATEMENT_TIMEOUT, a Go
	// duration), PostgreSQL's statement_timeout too.
	StatementTimeout time.Duration
	// DefaultRows is the row cap of a request that names none
	// (SQL_DEFAULT_ROWS); MaxRows the largest a request may ask
	// (SQL_MAX_ROWS).
	DefaultRows int
	MaxRows     int
	// MaxResultBytes caps the size of the rows of one result
	// (SQL_MAX_RESULT_BYTES): past it, the rows are dropped and the result
	// is truncated.
	MaxResultBytes int
	// MaxConcurrentPerUser is how many statements one user runs at once
	// (SQL_MAX_CONCURRENT_PER_USER); past it the request answers 429.
	MaxConcurrentPerUser int
	// AllowedNetworks are the CIDRs (or addresses) the editor and the
	// connection tests may reach although they are loopback, link-local or
	// the Kubernetes API server (SQL_ALLOWED_NETWORKS, comma-separated):
	// the platform's allow-list, empty by default.
	AllowedNetworks []string
	// CAFile is a PEM bundle of the CAs a database certificate is verified
	// against, besides the system's (SQL_CA_FILE): the platform's CA bundle,
	// global.okdp.tls.caBundleSecret, mounted by the chart.
	CAFile string
}

// SecretStoresConfig bounds the SecretStores the console creates in the
// projects' namespaces.
type SecretStoresConfig struct {
	// AllowedServers are the Vault servers a store may name beside the
	// platform's secret store's (global.okdp.secrets.provider.vault.server)
	// (SECRET_STORE_ALLOWED_SERVERS, comma-separated URLs).
	AllowedServers []string
}

const defaultSidecarPrefixes = "istio-proxy,istio-init,dynatrace-,linkerd-proxy,envoy,vault-agent"

// Load returns the configuration loaded from environment variables or defaults
func Load() (*Config, error) {
	cfg := &Config{
		ServerPort:        getEnv("PORT", "8093"),
		PlatformNamespace: getEnv("PLATFORM_NAMESPACE", "okdp-system"),
		AllowedOrigins:    getEnv("ALLOWED_ORIGINS", "http://localhost:4200"),
		LogLevel:          getEnv("LOG_LEVEL", "info"),
		OIDC: OIDCConfig{
			Issuer:      strings.TrimSpace(getEnv("OIDC_ISSUER", "")),
			ClientID:    strings.TrimSpace(getEnv("OIDC_CLIENT_ID", "")),
			GroupsClaim: strings.TrimSpace(getEnv("OIDC_GROUPS_CLAIM", "groups")),
			Disabled:    getEnv("AUTH_DISABLED", "") == "true",
		},
		GitOps: GitOpsConfig{
			CredentialsDir:        getEnv("GITOPS_CREDENTIALS_DIR", "/etc/okdp/gitops-credentials"),
			InsecureIgnoreHostKey: getEnv("GITOPS_SSH_INSECURE_IGNORE_HOST_KEY", "") == "true",
			InsecureHTTPHosts:     gitops.InsecureHTTPHosts(os.Getenv(gitops.InsecureHTTPHostsEnv)),
			AuthorName:            getEnv("GITOPS_AUTHOR_NAME", "OKDP control plane"),
			AuthorEmail:           getEnv("GITOPS_AUTHOR_EMAIL", "okdp-control-plane@okdp.io"),
			ConfigFile:            strings.TrimSpace(getEnv("GITOPS_CONFIG", "")),
			RemovedRepoURL:        strings.TrimSpace(getEnv("GITOPS_REPO_URL", "")),
			CloneDir:              getEnv("GITOPS_CLONE_DIR", ""),
			ChartCacheDir:         getEnv("GITOPS_CHART_CACHE_DIR", ""),
			Engine:                strings.ToLower(getEnv("GITOPS_ENGINE", EngineFlux)),
			ReleasesNamespace:     getEnv("GITOPS_RELEASES_NAMESPACE", "okdp-releases"),
			ArgoCDNamespace:       getEnv("ARGOCD_NAMESPACE", "argocd"),
		},
		KeycloakURL:          strings.TrimRight(strings.TrimSpace(getEnv("KEYCLOAK_URL", "")), "/"),
		KeycloakRealm:        strings.TrimSpace(getEnv("KEYCLOAK_REALM", "")),
		KeycloakClientID:     strings.TrimSpace(getEnv("KEYCLOAK_CLIENT_ID", "okdp-control-plane")),
		KeycloakClientSecret: getEnv("KEYCLOAK_CLIENT_SECRET", ""),
		KeycloakTLSInsecure:  getEnv("KEYCLOAK_TLS_INSECURE", "") == "true",
	}

	if cfg.GitOps.ChartCacheDir == "" {
		if cfg.GitOps.CloneDir != "" {
			cfg.GitOps.ChartCacheDir = filepath.Join(cfg.GitOps.CloneDir, "charts")
		} else {
			cfg.GitOps.ChartCacheDir = filepath.Join(os.TempDir(), "okdp-charts")
		}
	}

	sql, err := loadSQL()
	if err != nil {
		return nil, err
	}
	cfg.SQL = sql

	for _, u := range strings.Split(getEnv("SECRET_STORE_ALLOWED_SERVERS", ""), ",") {
		if trimmed := strings.TrimSpace(u); trimmed != "" {
			cfg.SecretStores.AllowedServers = append(cfg.SecretStores.AllowedServers, trimmed)
		}
	}

	for _, h := range strings.Split(getEnv("INSECURE_OCI_REGISTRIES", ""), ",") {
		if trimmed := strings.TrimSpace(h); trimmed != "" {
			cfg.InsecureOCIRegistries = append(cfg.InsecureOCIRegistries, trimmed)
		}
	}

	raw := getEnv("EXCLUDED_SIDECAR_PREFIXES", defaultSidecarPrefixes)
	for _, p := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			cfg.ExcludedSidecarPrefixes = append(cfg.ExcludedSidecarPrefixes, trimmed)
		}
	}

	return cfg, nil
}

// loadSQL reads the SQL_* settings, each with its default.
func loadSQL() (SQLConfig, error) {
	sql := SQLConfig{CAFile: strings.TrimSpace(getEnv("SQL_CA_FILE", ""))}
	timeout, err := time.ParseDuration(getEnv("SQL_STATEMENT_TIMEOUT", "2m"))
	if err != nil || timeout <= 0 {
		return sql, fmt.Errorf("SQL_STATEMENT_TIMEOUT: a positive duration, got %q", os.Getenv("SQL_STATEMENT_TIMEOUT"))
	}
	sql.StatementTimeout = timeout
	for _, n := range []struct {
		name     string
		fallback int
		into     *int
	}{
		{"SQL_DEFAULT_ROWS", 1000, &sql.DefaultRows},
		{"SQL_MAX_ROWS", 10000, &sql.MaxRows},
		{"SQL_MAX_RESULT_BYTES", 16 << 20, &sql.MaxResultBytes},
		{"SQL_MAX_CONCURRENT_PER_USER", 2, &sql.MaxConcurrentPerUser},
	} {
		v, err := strconv.Atoi(getEnv(n.name, strconv.Itoa(n.fallback)))
		if err != nil || v <= 0 {
			return sql, fmt.Errorf("%s: a positive integer, got %q", n.name, os.Getenv(n.name))
		}
		*n.into = v
	}
	if sql.DefaultRows > sql.MaxRows {
		sql.DefaultRows = sql.MaxRows
	}
	for _, n := range strings.Split(getEnv("SQL_ALLOWED_NETWORKS", ""), ",") {
		if trimmed := strings.TrimSpace(n); trimmed != "" {
			sql.AllowedNetworks = append(sql.AllowedNetworks, trimmed)
		}
	}
	return sql, nil
}

// getEnv retrieves an environment variable or returns a default value
func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}
