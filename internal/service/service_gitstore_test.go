package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/okdp/okdp-gitops/gitops"
	"github.com/okdp/okdp-gitops/treeedit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/models"
)

func init() {
	// Serve file:// in process: the tests need no git binary.
	client.InstallProtocol("file", server.DefaultServer)
}

// newGitStore returns two GitStores on the same fresh bare repository: the
// server's and another writer's.
func newGitStore(t *testing.T) (*gitops.GitStore, *gitops.GitStore) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	_, err := git.PlainInit(dir, true)
	require.NoError(t, err)
	open := func() *gitops.GitStore {
		s, err := gitops.NewGitStore(gitops.GitOptions{URL: "file://" + dir, Branch: "main", RefreshInterval: time.Nanosecond})
		require.NoError(t, err)
		return s
	}
	return open(), open()
}

// run fails the test when fn does not return in time: a deadlock on the
// store's lock must fail the test, not hang it.
func run(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("timed out: the write deadlocked on the deployments repository")
	}
}

func seedTrino(t *testing.T, store gitops.Store) *gitops.Deployments {
	t.Helper()
	d := gitops.NewDeployments(store)
	_, err := d.Store.Update(context.Background(), gitops.Commit{Subject: "seed"}, func(tx gitops.Tx) error {
		if err := tx.WriteFile(treeedit.CatalogsFile, []byte("categories: []\n")); err != nil {
			return err
		}
		return tx.WriteFile("projects/demo/project.yaml", []byte("{}\n"))
	})
	require.NoError(t, err)
	_, err = d.CreateInstance(context.Background(), auth.ActorFrom(aliceContext()), treeedit.Instance{
		Name: "trino", Project: "demo", Service: "trino", Version: "480.0.0-p21", Values: map[string]any{"workers": 2},
	})
	require.NoError(t, err)
	return d
}

// Regression: checking a parameters PATCH reads the tree from Git. Run
// inside the write callback, a read of the store waited for the lock the
// write held, forever, and every later Git read with it. The memory store
// hides this; a real GitStore does not. The check is the compile of the
// write, inside it.
func TestUpdateParametersDoesNotDeadlockTheGitStore(t *testing.T) {
	store, _ := newGitStore(t)
	d := seedTrino(t, store)
	compiledCatalog(t, d, trinoSchema())
	svc := NewDefaultServiceService(ServiceDeps{Deployments: d, PlatformRepo: stubPlatform{}})

	run(t, func() {
		_, err := svc.UpdateServiceParameters(aliceContext(), "demo", "trino", models.ServiceUpdateRequest{Parameters: map[string]any{"workers": float64(3)}})
		assert.NoError(t, err)
		_, err = svc.UpdateServiceParameters(aliceContext(), "demo", "trino", models.ServiceUpdateRequest{Parameters: map[string]any{"workers": float64(0)}})
		assert.True(t, IsValidationError(err), "got %v", err)
	})

	st, err := d.GetInstance(context.Background(), "demo", "trino")
	require.NoError(t, err)
	assert.EqualValues(t, 3, st.Values["workers"], "the valid patch is committed, the invalid one is not")
}

// A declaration another writer changed is patched as it is: the committed
// values are the patch applied to what the other writer left.
func TestUpdateParametersPatchesWhatAnotherWriterLeft(t *testing.T) {
	store, other := newGitStore(t)
	d := seedTrino(t, store)
	otherDeployments := gitops.NewDeployments(other)
	_, _, err := otherDeployments.UpdateInstance(context.Background(), auth.ActorFrom(aliceContext()), "demo", "trino", func(st *treeedit.Instance) error {
		st.Values["catalogs"] = []any{map[string]any{"name": "lake"}}
		return nil
	})
	require.NoError(t, err)
	svc := NewDefaultServiceService(ServiceDeps{Deployments: d, PlatformRepo: stubPlatform{}})

	run(t, func() {
		_, err := svc.UpdateServiceParameters(aliceContext(), "demo", "trino", models.ServiceUpdateRequest{Parameters: map[string]any{"workers": float64(4)}})
		assert.NoError(t, err)
	})

	st, err := d.GetInstance(context.Background(), "demo", "trino")
	require.NoError(t, err)
	assert.EqualValues(t, 4, st.Values["workers"])
	assert.Contains(t, st.Values, "catalogs", "the other writer's change is kept")
}
