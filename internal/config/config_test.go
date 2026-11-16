package config

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthenticationCanBeTurnedOffOnlyByName(t *testing.T) {
	t.Setenv("AUTH_DISABLED", "true")

	cfg, err := Load()

	require.NoError(t, err)
	assert.True(t, cfg.OIDC.Disabled)
}

func TestAuthenticationIsOnUnlessTurnedOff(t *testing.T) {
	t.Setenv("AUTH_DISABLED", "")

	cfg, err := Load()

	require.NoError(t, err)
	assert.False(t, cfg.OIDC.Disabled)
}

func TestTheIssuerIsNotSetFromTheEnvironmentByDefault(t *testing.T) {
	cfg, err := Load()

	require.NoError(t, err)
	assert.Empty(t, cfg.OIDC.Issuer, "the issuer normally comes from the platform values")
}

func TestTheChartCacheIsUnderTheCloneDirectory(t *testing.T) {
	t.Setenv("GITOPS_CLONE_DIR", "/var/cache/okdp")
	t.Setenv("GITOPS_CHART_CACHE_DIR", "")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "/var/cache/okdp/charts", cfg.GitOps.ChartCacheDir)

	t.Setenv("GITOPS_CHART_CACHE_DIR", "/charts")
	cfg, err = Load()
	require.NoError(t, err)
	assert.Equal(t, "/charts", cfg.GitOps.ChartCacheDir)
}

func TestGitOpsDefaultsAndValidation(t *testing.T) {
	t.Setenv("GITOPS_CONFIG", "")
	t.Setenv("GITOPS_REPO_URL", "")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, EngineFlux, cfg.GitOps.Engine)
	assert.Equal(t, "okdp-releases", cfg.GitOps.ReleasesNamespace)
	assert.ErrorContains(t, cfg.GitOps.Validate(), "GITOPS_CONFIG is required", "no topology, no desired state")

	t.Setenv("GITOPS_CONFIG", "/etc/okdp/gitops/topology.yaml")
	t.Setenv("GITOPS_ENGINE", "ArgoCD")
	cfg, err = Load()
	require.NoError(t, err)
	assert.NoError(t, cfg.GitOps.Validate())
	assert.Equal(t, EngineArgoCD, cfg.GitOps.Engine)

	t.Setenv("GITOPS_ENGINE", "kubocd")
	cfg, _ = Load()
	assert.Error(t, cfg.GitOps.Validate())
}

func TestTheSingleRepositoryModeIsRefused(t *testing.T) {
	t.Setenv("GITOPS_REPO_URL", "https://git.example.com/okdp/deployments.git")
	t.Setenv("GITOPS_CONFIG", "")
	cfg, err := Load()
	require.NoError(t, err)
	assert.ErrorContains(t, cfg.GitOps.Validate(), "GITOPS_REPO_URL is no longer read")

	t.Setenv("GITOPS_CONFIG", "/etc/okdp/gitops/topology.yaml")
	cfg, err = Load()
	require.NoError(t, err)
	assert.ErrorContains(t, cfg.GitOps.Validate(), "GITOPS_REPO_URL is no longer read", "not ignored beside a topology")
}

func TestTheTopologyFileNamesTheRepositories(t *testing.T) {
	t.Setenv("GITOPS_REPO_URL", "")
	file := t.TempDir() + "/topology.yaml"
	require.NoError(t, os.WriteFile(file, []byte("platform: {url: https://git/okdp-platform.git, path: \"\"}\nprojects: {url: \"https://git/okdp-project-{project}.git\", path: \"\"}\noutput: {url: https://git/okdp-output.git}\n"), 0o644))
	t.Setenv("GITOPS_CONFIG", file)
	cfg, err := Load()
	require.NoError(t, err)
	require.NoError(t, cfg.GitOps.Validate())
	topology, err := cfg.GitOps.Topology()
	require.NoError(t, err)
	assert.True(t, topology.Split())
	demo, err := topology.Project("demo", nil)
	require.NoError(t, err)
	assert.Equal(t, "https://git/okdp-project-demo.git", demo.URL)

	// The output in a repository of the sources: no topology of one
	// repository.
	require.NoError(t, os.WriteFile(file, []byte("platform: {url: https://git/deployments.git}\nprojects: {url: https://git/deployments.git, path: \"projects/{project}\"}\noutput: {url: https://git/deployments.git}\n"), 0o644))
	_, err = cfg.GitOps.Topology()
	assert.ErrorContains(t, err, "repository of its own")
}

func TestSQLDefaultsAndValidation(t *testing.T) {
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, SQLConfig{StatementTimeout: 2 * time.Minute, DefaultRows: 1000, MaxRows: 10000, MaxResultBytes: 16 << 20, MaxConcurrentPerUser: 2}, cfg.SQL)

	t.Setenv("SQL_ALLOWED_NETWORKS", " 10.0.0.0/8, ,127.0.0.1")
	t.Setenv("SQL_MAX_ROWS", "500")
	cfg, err = Load()
	require.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.0/8", "127.0.0.1"}, cfg.SQL.AllowedNetworks)
	assert.Equal(t, 500, cfg.SQL.DefaultRows, "the default row cap never passes the maximum")

	t.Setenv("SQL_STATEMENT_TIMEOUT", "soon")
	_, err = Load()
	assert.ErrorContains(t, err, "SQL_STATEMENT_TIMEOUT")
}

// OKDP_CA_FILE is the CA bundle of every TLS client; SQL_CA_FILE, which
// the SQL editor alone read, is refused rather than ignored.
func TestTheCABundleIsOKDPCAFile(t *testing.T) {
	t.Setenv("GITOPS_CONFIG", "/etc/okdp/gitops/topology.yaml")
	t.Setenv("OKDP_CA_FILE", " /etc/okdp/ca/ca.crt ")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "/etc/okdp/ca/ca.crt", cfg.CAFile)

	t.Setenv("SQL_CA_FILE", "/etc/okdp/sql-ca/ca.crt")
	_, err = Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OKDP_CA_FILE")
}

// Each insecure setting of the environment is named, for the startup
// warning; none by default.
func TestInsecureSettingsAreNamed(t *testing.T) {
	cfg, err := Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.InsecureSettings())

	t.Setenv("KEYCLOAK_TLS_INSECURE", "true")
	t.Setenv("GITOPS_SSH_INSECURE_IGNORE_HOST_KEY", "true")
	t.Setenv("GITOPS_INSECURE_HTTP_HOSTS", "forgejo-http.forgejo")
	t.Setenv("INSECURE_OCI_REGISTRIES", "registry.local:5000")
	cfg, err = Load()
	require.NoError(t, err)
	settings := cfg.InsecureSettings()
	require.Len(t, settings, 4)
	assert.Contains(t, settings[0], "KEYCLOAK_TLS_INSECURE=true")
	assert.Contains(t, settings[1], "GITOPS_SSH_INSECURE_IGNORE_HOST_KEY=true")
	assert.Contains(t, settings[2], "GITOPS_INSECURE_HTTP_HOSTS=forgejo-http.forgejo")
	assert.Contains(t, settings[3], "INSECURE_OCI_REGISTRIES=registry.local:5000")
}
