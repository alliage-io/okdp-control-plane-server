package service

import (
	"context"
	"os"
	"testing"

	compiler "github.com/okdp/okdp-compiler"
	"github.com/okdp/okdp-compiler/treeedit"
	"github.com/okdp/okdp-gitops/catalog"
	"github.com/okdp/okdp-gitops/gitops"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

// claimsCatalog is the claims example of okdp-compiler: a database server
// that provisions database-server connections, and an application that
// claims one.
const claimsCatalog = "testdata/catalog"

// claimsLayout is a tree of that catalog: a database server, the instance
// postgresql of the project data, the provider of the platform's default
// class of database-server connections and of the project's class large,
// which any project may claim of (claimers: ['*']);
// the project demo, its named claim analytics, app claiming a database of
// its own, reports naming analytics.
func claimsLayout(extra map[string]string) *gitops.MemoryStore {
	files := map[string]string{
		treeedit.CatalogsFile: "repositories:\n  - {name: test, url: https://git.example.org/test.git, ref: main}\n",
		compiler.PlatformFile: `global:
  okdp:
    provisioning:
      database-server:
        default: standard
        classes:
          standard: {provider: data/postgresql}
    secrets:
      store: okdp-secrets
`,
		"projects/data/project.yaml":                       "{}\n",
		"projects/data/instances/postgresql/instance.yaml": "service: postgresql\nversion: 1.0.0\nclaimers: ['*']\n",
		"projects/demo/project.yaml": `# The demo project.
global:
  okdp:
    provisioning:
      database-server:
        classes:
          large: {provider: data/postgresql, request: {size: 50Gi}}
# Its named claims.
claims:
  analytics:
    contract: database-server
    request: {extensions: [postgis]}
`,
		"projects/demo/instances/app/instance.yaml":     "service: app\nversion: 1.0.0\n",
		"projects/demo/instances/reports/instance.yaml": "service: app\nversion: 1.0.0\n",
		"projects/demo/instances/reports/config.yaml":   "database: analytics\n",
	}
	for k, v := range extra {
		files[k] = v
	}
	return gitops.MustMemoryStore(files)
}

// claimsDeployments are the deployments of store, compiled with the claims
// catalog.
func claimsDeployments(store gitops.Store) *gitops.Deployments {
	d := gitops.NewDeployments(store)
	d.Compiler = gitops.NewCompiler(&catalog.Loader{Dirs: map[string]string{"test": claimsCatalog}})
	return d
}

func newClaimServiceUnderTest(t *testing.T, store gitops.Store) *DefaultClaimService {
	t.Helper()
	contracts, err := NewContractCatalog(os.DirFS(claimsCatalog + "/contracts"))
	require.NoError(t, err)
	return NewDefaultClaimService(claimsDeployments(store), contracts)
}

func TestProvisioningListsThePlatformAndTheProjectClasses(t *testing.T) {
	svc := newClaimServiceUnderTest(t, claimsLayout(nil))
	got, err := svc.Provisioning(context.Background(), "demo")
	require.NoError(t, err)
	require.Len(t, got.Contracts, 1)
	db := got.Contracts[0]
	assert.Equal(t, "database-server", db.Contract)
	assert.Equal(t, "standard", db.Default)
	assert.Equal(t, models.ClassLevelPlatform, db.DefaultLevel)
	require.Len(t, db.Classes, 2)
	assert.Equal(t, models.ProvisioningClass{Name: "standard", Provider: "data/postgresql", Level: models.ClassLevelPlatform}, db.Classes[0])
	assert.Equal(t, "large", db.Classes[1].Name)
	assert.Equal(t, models.ClassLevelProject, db.Classes[1].Level)
	assert.Equal(t, map[string]any{"size": "50Gi"}, db.Classes[1].Request)
	assert.Contains(t, db.ClaimSchema["properties"], "extensions")
}

func TestNamedClaimsAreBoundAndNamed(t *testing.T) {
	svc := newClaimServiceUnderTest(t, claimsLayout(nil))
	claims, err := svc.ListNamedClaims(context.Background(), "demo")
	require.NoError(t, err)
	require.Len(t, claims, 1)
	c := claims[0]
	assert.Equal(t, "analytics", c.Name)
	assert.Equal(t, "demo.analytics", c.ID)
	assert.Equal(t, "database-server", c.Contract)
	require.NotNil(t, c.Binding)
	assert.Equal(t, models.ClaimBinding{Namespace: "data", Release: "data-postgresql", Instance: "postgresql", Class: "standard", Default: true}, *c.Binding)
	assert.Equal(t, []string{"reports"}, c.Users)
	assert.Equal(t, "postgresql", c.Engine, "the engine its provider granted")
	assert.True(t, c.Sql, "a PostgreSQL database: the SQL editor runs on it")

	_, err = svc.ListNamedClaims(context.Background(), "nope")
	assert.True(t, apierrors.IsNotFound(err))
}

func TestANamedClaimIsDeclaredInProjectYAMLItsCommentsKept(t *testing.T) {
	store := claimsLayout(nil)
	svc := newClaimServiceUnderTest(t, store)
	view, err := svc.CreateNamedClaim(context.Background(), "demo", models.NamedClaimRequest{
		Name: "metrics", Contract: "database-server", Class: "large", Request: map[string]any{"extensions": []any{"pgvector"}},
	})
	require.NoError(t, err)
	assert.Equal(t, "demo.metrics", view.ID)
	require.NotNil(t, view.Binding, "a named claim binds whether an instance names it or not")
	assert.Equal(t, "large", view.Binding.Class)

	raw := store.Files()["projects/demo/project.yaml"]
	assert.Contains(t, raw, "# The demo project.")
	assert.Contains(t, raw, "# Its named claims.")
	assert.Contains(t, raw, "  metrics:\n    contract: database-server\n    class: large\n    request:\n      extensions:\n        - pgvector\n")

	_, err = svc.CreateNamedClaim(context.Background(), "demo", models.NamedClaimRequest{Name: "metrics", Contract: "database-server"})
	assert.True(t, apierrors.IsAlreadyExists(err), "%v", err)

	view, err = svc.UpdateNamedClaim(context.Background(), "demo", "metrics", models.NamedClaimRequest{Contract: "database-server"})
	require.NoError(t, err)
	assert.Empty(t, view.Class)
	assert.True(t, view.Binding.Default)

	require.NoError(t, svc.DeleteNamedClaim(context.Background(), "demo", "metrics"))
	assert.NotContains(t, store.Files()["projects/demo/project.yaml"], "metrics")
	err = svc.DeleteNamedClaim(context.Background(), "demo", "metrics")
	assert.True(t, apierrors.IsNotFound(err), "%v", err)
}

func TestANamedClaimIsRefusedWhenItCannotBind(t *testing.T) {
	svc := newClaimServiceUnderTest(t, claimsLayout(nil))
	for name, req := range map[string]models.NamedClaimRequest{
		"unknown class":      {Name: "x", Contract: "database-server", Class: "huge"},
		"class and provider": {Name: "x", Contract: "database-server", Class: "large", Provider: "data/postgresql"},
		"a request refused":  {Name: "x", Contract: "database-server", Request: map[string]any{"extensions": []any{"postgis2"}}},
		"a key of the class": {Name: "x", Contract: "database-server", Class: "large", Request: map[string]any{"size": "1Gi"}},
		"unknown contract":   {Name: "x", Contract: "nope"},
		"a name":             {Name: "X_y", Contract: "database-server"},
	} {
		_, err := svc.CreateNamedClaim(context.Background(), "demo", req)
		assert.True(t, IsValidationError(err), "%s: %v", name, err)
	}
}

func TestANamedClaimAnInstanceNamesIsKept(t *testing.T) {
	svc := newClaimServiceUnderTest(t, claimsLayout(nil))
	err := svc.DeleteNamedClaim(context.Background(), "demo", "analytics")
	require.True(t, IsValidationError(err), "%v", err)
	assert.Contains(t, err.Error(), "still named by reports")
}

func TestInstanceClaimsGivesTheBindingsAndWhatAProviderGranted(t *testing.T) {
	svc := newClaimServiceUnderTest(t, claimsLayout(map[string]string{
		// A project instance that provisions database-server, named by a
		// claim of app.
		"projects/demo/instances/pg/instance.yaml":    "service: postgresql\nversion: 1.0.0\n",
		"projects/demo/instances/app/config.yaml":     "database: {provider: pg, request: {size: 5Gi}}\n",
		"projects/demo/instances/other/instance.yaml": "service: app\nversion: 1.0.0\n",
	}))
	app, err := svc.InstanceClaims(context.Background(), "demo", "app")
	require.NoError(t, err)
	assert.Empty(t, app.Error)
	require.Len(t, app.Bound, 1)
	b := app.Bound[0]
	assert.Equal(t, "database", b.Field)
	assert.Equal(t, "demo.app.database", b.ID)
	assert.Equal(t, models.ClaimBinding{Namespace: "demo", Release: "demo-pg", Instance: "pg"}, b.Binding)
	require.NotNil(t, b.Granted)
	assert.Equal(t, "5Gi", b.Granted.Values["size"])

	pg, err := svc.InstanceClaims(context.Background(), "demo", "pg")
	require.NoError(t, err)
	assert.Equal(t, []string{"database-server"}, pg.Provisions)
	require.Len(t, pg.Provided, 1)
	assert.Equal(t, "demo.app.database", pg.Provided[0].ID)
	assert.Equal(t, []models.ClaimUser{{Namespace: "demo", Instance: "app", Release: "demo-app", Field: "database"}}, pg.Provided[0].Claimers)
	assert.Equal(t, "demo.app.database-credentials", pg.Provided[0].Granted.SecretRef)

	reports, err := svc.InstanceClaims(context.Background(), "demo", "reports")
	require.NoError(t, err)
	require.Len(t, reports.Bound, 1)
	assert.Equal(t, "analytics", reports.Bound[0].Name)
	assert.Equal(t, "postgresql", reports.Bound[0].Binding.Instance)
}

// The claims of an instance's parameters are the compiler's to check, on
// the write: each refusal at the field of its reference.
func TestTheClaimsOfTheParametersAreRefusedAtTheirField(t *testing.T) {
	store := claimsLayout(nil)
	d := claimsDeployments(store)
	for want, values := range map[string]map[string]any{
		`class "huge" is not a class`: {"database": map[string]any{"class": "huge"}},
		"database":                    {"database": map[string]any{"size": "1Gi"}},
		"a/b/c":                       {"database": map[string]any{"provider": "a/b/c"}},
	} {
		_, err := d.CreateInstance(context.Background(), gitops.Actor{}, treeedit.Instance{Name: "x", Project: "demo", Service: "app", Version: "1.0.0", Values: values})
		var refused *treeedit.CompileError
		require.ErrorAs(t, err, &refused, want)
		fields := ValidationFields(DoesNotCompile(refused))
		require.NotEmpty(t, fields, want)
		assert.Equal(t, "database", fields[0].Path, "%s: %+v", want, fields)
		assert.Equal(t, "/database", fields[0].Pointer)
		assert.Contains(t, fields[0].Message, want)
	}
}

// The write check compiles the tree together: an instance that claims its
// database is checked with its claim bound, across namespaces.
func TestTheWriteCheckBindsTheClaimsOfAnInstance(t *testing.T) {
	store := claimsLayout(nil)
	d := claimsDeployments(store)
	newInstance := func(name string, values map[string]any) treeedit.Instance {
		return treeedit.Instance{Name: name, Project: "demo", Service: "app", Version: "1.0.0", Values: values}
	}
	_, err := d.CreateInstance(context.Background(), gitops.Actor{}, newInstance("metrics", map[string]any{
		"database": map[string]any{"class": "large", "request": map[string]any{"extensions": []any{"timescaledb"}}},
	}))
	require.NoError(t, err)

	_, err = d.CreateInstance(context.Background(), gitops.Actor{}, newInstance("broken", map[string]any{
		"database": map[string]any{"class": "huge"},
	}))
	var refused *treeedit.CompileError
	require.ErrorAs(t, err, &refused)
	assert.Contains(t, err.Error(), `class "huge" is not a class of database-server connections`)
	assert.NotContains(t, store.Files(), "projects/demo/instances/broken/instance.yaml")
}
