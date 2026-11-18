package service

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	compiler "github.com/okdp/okdp-compiler"
	"github.com/okdp/okdp-compiler/chart"
	"github.com/okdp/okdp-gitops/catalog"
	"github.com/okdp/okdp-gitops/gitops"
	"github.com/okdp/okdp-gitops/treeedit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
)

// renderCharts is the chart cache of the package app: a chart any render
// of which fails, so that a compile that passes did not render it.
func renderCharts() fstest.MapFS {
	dir := "charts.example.org/app-1.0.0/app"
	return fstest.MapFS{
		dir + "/Chart.yaml":       {Data: []byte("apiVersion: v2\nname: app\nversion: 1.0.0\n")},
		dir + "/values.yaml":      {Data: []byte("{}\n")},
		dir + "/templates/t.yaml": {Data: []byte(`{{ fail "the chart was rendered" }}`)},
	}
}

// catalogWithApp writes the package app of the chart of renderCharts and
// the server's compiler reading it with charts (NewTreeCompiler, as
// main.go), a project demo and its platform's ingress suffix.
func catalogWithApp(t *testing.T, d *gitops.Deployments, charts compiler.ChartSource) {
	t.Helper()
	cat := t.TempDir()
	pkg := filepath.Join(cat, "catalog", "app", "1.0.0")
	require.NoError(t, os.MkdirAll(pkg, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pkg, "Chart.yaml"), []byte("apiVersion: v2\nname: app\nversion: 1.0.0\ndependencies:\n  - {name: app, repository: https://charts.example.org, version: 1.0.0}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pkg, "okdp.yaml"), []byte("upstream:\n  app: {}\n"), 0o644))
	d.Compiler = NewTreeCompiler(&catalog.Loader{Dirs: map[string]string{"test": cat}}, charts)
	_, err := d.Store.Update(context.Background(), gitops.Commit{Subject: "the catalogs"}, func(tx gitops.Tx) error {
		for path, content := range map[string]string{
			treeedit.CatalogsFile:        "repositories:\n  - {name: test, url: https://git.example.org/test.git}\n",
			"platform.yaml":              "global:\n  okdp:\n    ingress: {suffix: okdp.sandbox}\n",
			"projects/demo/project.yaml": "{}\n",
		} {
			if err := tx.WriteFile(path, []byte(content)); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
}

var renderActor = auth.Actor{Username: "alice"}

func upstreamInstance(name string, upstream map[string]any) treeedit.Instance {
	return treeedit.Instance{Project: "demo", Name: name, Service: "app", Version: "1.0.0", Values: map[string]any{
		"upstream": map[string]any{"app": upstream},
	}}
}

// The server's compile does not render: a write and a read of a tree whose
// instance gives upstream values to a chart that fails at any render pass,
// the render is the CI's; what the compile checks of the values itself is
// still refused, not committed.
func TestTheServerDoesNotRender(t *testing.T) {
	svc, store := newGitServiceUnderTest(t, stubEngine{}, nil)
	catalogWithApp(t, svc.deployments, compiler.StaticCharts(renderCharts()))
	before := len(store.Messages)

	_, err := svc.deployments.CreateInstance(context.Background(), renderActor, upstreamInstance("web", map[string]any{"something": 1}))
	require.NoError(t, err, "the write's check does not render")
	assert.Len(t, store.Messages, before+1)

	tree, err := svc.deployments.Tree(context.Background())
	require.NoError(t, err)
	require.NotNil(t, tree.Instance("demo", "web"))
	assert.NoError(t, tree.Instance("demo", "web").Err(), "nor does the read's compile")

	_, err = svc.deployments.CreateInstance(context.Background(), renderActor, upstreamInstance("denied", map[string]any{"extraResources": []any{map[string]any{"kind": "ConfigMap"}}}))
	var refused *treeedit.CompileError
	require.True(t, errors.As(err, &refused), "got %v", err)
	assert.Contains(t, err.Error(), "extraResources is denied")
	assert.Len(t, store.Messages, before+1)
}

// A chart that could not be fetched refuses no write: no render needs it
// here, the CI's compile says it (a chart registry blip fails no edit).
func TestAWriteWithAnUnfetchableChartIsNotRefused(t *testing.T) {
	svc, store := newGitServiceUnderTest(t, stubEngine{}, nil)
	catalogWithApp(t, svc.deployments, unfetchableCharts{})
	before := len(store.Messages)

	_, err := svc.deployments.CreateInstance(context.Background(), renderActor, upstreamInstance("web", map[string]any{"something": 1}))
	require.NoError(t, err)
	assert.Len(t, store.Messages, before+1)
}

type unfetchableCharts struct{}

func (unfetchableCharts) Ensure(_ context.Context, refs []chart.Ref) (fs.FS, map[chart.Ref]error) {
	failures := map[chart.Ref]error{}
	for _, ref := range refs {
		failures[ref] = errors.New("the registry is down")
	}
	return fstest.MapFS{}, failures
}
