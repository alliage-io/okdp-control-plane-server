package service

import (
	"context"
	"errors"
	"testing"

	"github.com/okdp/okdp-gitops/gitops"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/okdp/okdp-control-plane-server/internal/apperr"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	"github.com/okdp/okdp-control-plane-server/internal/service/mocks"
)

// ctxStore is a store that sees its context, as the Git store does: a
// cancelled one is refused.
type ctxStore struct {
	*gitops.MemoryStore
}

func (s ctxStore) Update(ctx context.Context, commit gitops.Commit, apply func(tx gitops.Tx) error) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return s.MemoryStore.Update(ctx, commit, apply)
}

// The client gone once the connection is committed: the failed Secret
// write's rollback still reaches the store, which refuses the request's
// cancelled context (SRV-25).
func TestACreateWhoseClientIsGoneIsStillTakenBack(t *testing.T) {
	svc, env, store := newServiceUnderTest(t)
	env.deployments.Store = ctxStore{store}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env.secrets.onPut = func(string, string) {
		cancel()
		env.secrets.failPut = errors.New("the request's context is cancelled")
	}

	_, err := svc.Create(ctx, "demo", postgresRequest())
	require.Error(t, err)
	assert.NotContains(t, store.Files()["projects/demo/project.yaml"], "warehouse")
}

func TestAnUpdateWhoseClientIsGoneIsStillRestored(t *testing.T) {
	svc, env, store := newServiceUnderTest(t)
	_, err := svc.Create(context.Background(), "demo", postgresRequest())
	require.NoError(t, err)

	env.deployments.Store = ctxStore{store}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env.secrets.onPut = func(string, string) {
		cancel()
		env.secrets.failPut = errors.New("the request's context is cancelled")
	}
	edit := postgresRequest()
	edit.Values["port"] = float64(6432)
	edit.Values["password"] = "rotated"
	_, err = svc.Update(ctx, "demo", "warehouse", edit)
	require.Error(t, err)

	file := store.Files()["projects/demo/project.yaml"]
	assert.Contains(t, file, "port: 5432")
	assert.NotContains(t, file, "port: 6432")
}

// A create answering 409 changes nothing: neither the store's token nor
// the default label of another store (SRV-24).
func TestACreateSecretStoreOnATakenNameChangesNothing(t *testing.T) {
	repo := &mocks.SecretStoreRepository{}
	repo.On("Create", mock.Anything, "demo", mock.Anything).Return(apperr.AlreadyExists("secretstores", "store"))
	svc := NewDefaultSecretStoreService(repo, nil, nil)

	req := tokenRequest("https://vault.example.com", "t")
	req.IsDefault = true
	_, err := svc.CreateSecretStore(context.Background(), "demo", req)

	require.True(t, errors.Is(err, apperr.ErrAlreadyExists), "got %v", err)
	repo.AssertNotCalled(t, "CreateOrUpdateSecret", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	repo.AssertNotCalled(t, "RemoveDefaultLabel", mock.Anything, mock.Anything, mock.Anything)
	repo.AssertNotCalled(t, "Delete", mock.Anything, mock.Anything, mock.Anything)
}

// Created: the Secret and the labels follow, the other stores' default
// label cleared, not the new store's.
func TestACreatedSecretStoreKeepsItsDefaultLabel(t *testing.T) {
	repo := &mocks.SecretStoreRepository{}
	var calls []string
	repo.On("Create", mock.Anything, "demo", mock.Anything).Run(func(mock.Arguments) { calls = append(calls, "create") }).Return(nil)
	repo.On("CreateOrUpdateSecret", mock.Anything, "demo", "store-credentials", mock.Anything).Run(func(mock.Arguments) { calls = append(calls, "secret") }).Return(nil)
	repo.On("RemoveDefaultLabel", mock.Anything, "demo", "store").Run(func(mock.Arguments) { calls = append(calls, "label") }).Return(nil)
	repo.On("Get", mock.Anything, "demo", "store").Return(&crd.ESOSecretStore{}, nil)
	svc := NewDefaultSecretStoreService(repo, nil, nil)

	req := tokenRequest("https://vault.example.com", "t")
	req.IsDefault = true
	_, err := svc.CreateSecretStore(context.Background(), "demo", req)

	require.NoError(t, err)
	assert.Equal(t, []string{"create", "secret", "label"}, calls)
}

// A store whose Secret cannot be written is taken back, on a context the
// client's cancellation does not reach (SRV-25).
func TestACreatedSecretStoreWhoseSecretFailsIsTakenBack(t *testing.T) {
	repo := &mocks.SecretStoreRepository{}
	ctx, cancel := context.WithCancel(context.Background())
	repo.On("Create", mock.Anything, "demo", mock.Anything).Run(func(mock.Arguments) { cancel() }).Return(nil)
	repo.On("CreateOrUpdateSecret", mock.Anything, "demo", "store-credentials", mock.Anything).Return(errors.New("apiserver down"))
	repo.On("Delete", mock.MatchedBy(func(c context.Context) bool { return c.Err() == nil }), "demo", "store").Return(nil)
	svc := NewDefaultSecretStoreService(repo, nil, nil)

	_, err := svc.CreateSecretStore(ctx, "demo", tokenRequest("https://vault.example.com", "t"))

	require.Error(t, err)
	repo.AssertExpectations(t)
}
