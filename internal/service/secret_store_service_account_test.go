package service

import (
	"context"
	"reflect"
	"testing"

	compiler "github.com/okdp/okdp-compiler"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	"github.com/okdp/okdp-control-plane-server/internal/service/mocks"
	"github.com/stretchr/testify/mock"
)

func serviceAccountPtr(name string) *string { return &name }

func kubernetesAuth(serviceAccount *string) *models.SecretStoreAuth {
	return &models.SecretStoreAuth{
		Type: "kubernetes",
		Config: models.SecretAuthConfig{
			Role:           "demo-role",
			MountPath:      "kubernetes",
			ServiceAccount: serviceAccount,
		},
	}
}

// sandboxStore is the platform's secret store as the compile gives it:
// the sandbox's ServiceAccount okdp-secrets and audience openbao.
var sandboxStore = &platformSecretStore{server: "https://vault.example.com", serviceAccount: "okdp-secrets", audiences: []string{"openbao"}}

func storeRequest(server string, auth *models.SecretStoreAuth) models.SecretStoreRequest {
	return models.SecretStoreRequest{
		Name:     "store",
		Provider: "vault",
		Vault:    &models.VaultConfig{Server: server, Path: "secret", Version: "v2"},
		Auth:     auth,
	}
}

// created runs a create with the platform's secret store platform and
// returns the CR handed to the repository, nil when none was.
func created(t *testing.T, platform *platformSecretStore, allowed []string, req models.SecretStoreRequest) (*crd.ESOSecretStore, error) {
	t.Helper()
	repo := &mocks.SecretStoreRepository{}
	var sent *crd.ESOSecretStore
	repo.On("Create", mock.Anything, "demo", mock.Anything).Run(func(args mock.Arguments) {
		sent = args.Get(2).(*crd.ESOSecretStore)
	}).Return(nil).Maybe()
	repo.On("Get", mock.Anything, "demo", "store").Return(kubernetesStored("okdp-secrets"), nil).Maybe()
	svc := &DefaultSecretStoreService{repo: repo, allowedServers: allowed, platform: func(context.Context) *platformSecretStore { return platform }}
	_, err := svc.CreateSecretStore(context.Background(), "demo", req)
	return sent, err
}

// External Secrets mints a token of the store's ServiceAccount and sends it
// to the store's server: the account is the platform's store's, which the
// compiled manifests create in the namespace, and the token's audiences
// the platform's, never the API server's. A request naming that account,
// or none, gets it; another is refused, nothing created.
func TestKubernetesAuthAuthenticatesAsThePlatformsServiceAccount(t *testing.T) {
	for _, sa := range []*string{nil, serviceAccountPtr(""), serviceAccountPtr("okdp-secrets")} {
		sent, err := created(t, sandboxStore, nil, storeRequest("https://vault.example.com", kubernetesAuth(sa)))
		if err != nil || sent == nil {
			t.Fatalf("serviceAccount %v: %v", sa, err)
		}
		ref := sent.Spec.Provider.Vault.Auth.Kubernetes.ServiceAccountRef
		if ref == nil || ref.Name != "okdp-secrets" || len(ref.Audiences) != 1 || ref.Audiences[0] != "openbao" {
			t.Fatalf("serviceAccountRef %+v, want okdp-secrets of audience openbao", ref)
		}
	}
	for _, sa := range []string{"default", "vault-reader"} {
		sent, err := created(t, sandboxStore, nil, storeRequest("https://vault.example.com", kubernetesAuth(serviceAccountPtr(sa))))
		if !IsValidationError(err) || sent != nil {
			t.Fatalf("serviceAccount %s: %v, created %v", sa, err, sent != nil)
		}
	}
}

// The store's server is the platform's secret store's, or one of the
// control plane's allowedServers, compared as URLs; any other is refused.
func TestKubernetesAuthNamesAnAllowedServerOnly(t *testing.T) {
	for _, server := range []string{"https://vault.example.com", "HTTPS://Vault.Example.com:443/", "https://other.example.com/v"} {
		if _, err := created(t, sandboxStore, []string{"https://other.example.com/v/"}, storeRequest(server, kubernetesAuth(nil))); err != nil {
			t.Errorf("%s: %v", server, err)
		}
	}
	for _, server := range []string{"https://evil.example.com", "https://vault.example.com:8200", "http://vault.example.com", "https://user@vault.example.com", "https://vault.example.com/?x=1", "ftp://vault.example.com"} {
		sent, err := created(t, sandboxStore, nil, storeRequest(server, kubernetesAuth(nil)))
		if !IsValidationError(err) || sent != nil {
			t.Errorf("%s: %v, created %v", server, err, sent != nil)
		}
	}
}

// Without the platform's secret store, or with one of no audiences, a
// store of kubernetes auth is refused: there is no account of the
// platform's to authenticate as, or its token would be the API server's.
func TestKubernetesAuthNeedsThePlatformsSecretStore(t *testing.T) {
	noAudiences := &platformSecretStore{server: "https://vault.example.com", serviceAccount: "okdp-secrets"}
	for name, platform := range map[string]*platformSecretStore{"no store": nil, "no audiences": noAudiences} {
		sent, err := created(t, platform, []string{"https://vault.example.com"}, storeRequest("https://vault.example.com", kubernetesAuth(nil)))
		if !IsValidationError(err) || sent != nil {
			t.Errorf("%s: %v, created %v", name, err, sent != nil)
		}
	}
}

// The platform's secret store is read off the compile: a result's
// SecretStore, its provider the platform's text.
func TestThePlatformsSecretStoreIsTheCompiles(t *testing.T) {
	res := &compiler.Result{}
	ss := reflect.New(reflect.TypeOf(res.SecretStore).Elem())
	ss.Elem().FieldByName("Name").SetString("okdp-secrets")
	ss.Elem().FieldByName("ServiceAccount").SetString("okdp-secrets")
	ss.Elem().FieldByName("Provider").Set(reflect.ValueOf(map[string]any{"vault": map[string]any{
		"server": "https://openbao.openbao.svc.cluster.local:8200",
		"auth":   map[string]any{"kubernetes": map[string]any{"serviceAccountRef": map[string]any{"name": "okdp-secrets", "audiences": []any{"openbao"}}}},
	}}))
	reflect.ValueOf(res).Elem().FieldByName("SecretStore").Set(ss)
	refused := &compiler.TreeInstance{}
	compiled := &compiler.TreeInstance{}
	compiled.Outcome.Result = res
	svc := &DefaultSecretStoreService{tree: func(context.Context) (*compiler.Tree, error) {
		return &compiler.Tree{Instances: []*compiler.TreeInstance{refused, compiled}}, nil
	}}
	got := svc.platformStore(context.Background())
	if got == nil || got.server != "https://openbao.openbao.svc.cluster.local:8200" || got.serviceAccount != "okdp-secrets" || len(got.audiences) != 1 || got.audiences[0] != "openbao" {
		t.Fatalf("platform store %+v", got)
	}

	noProvider := &compiler.TreeInstance{}
	noProvider.Outcome.Result = &compiler.Result{}
	svc.tree = func(context.Context) (*compiler.Tree, error) {
		return &compiler.Tree{Instances: []*compiler.TreeInstance{noProvider, compiled}}, nil
	}
	if got := svc.platformStore(context.Background()); got != nil {
		t.Fatalf("a platform of no provider: %+v", got)
	}
}

// Reading the account back matters as much as writing it: the console
// shows which identity the Vault role must bind.
func TestResponseReportsTheServiceAccount(t *testing.T) {
	store := buildSecretStoreCRD("demo", storeRequest("https://vault.example.com", kubernetesAuth(nil)), "", sandboxStore)

	svc := &DefaultSecretStoreService{}
	resp := svc.toResponse(store, "demo")

	if resp.Auth == nil || resp.Auth.Config.ServiceAccount == nil {
		t.Fatal("the response carries no service account")
	}
	if got := *resp.Auth.Config.ServiceAccount; got != "okdp-secrets" {
		t.Fatalf("response reports %q, want okdp-secrets", got)
	}
}

// storedStore is a SecretStore as the cluster already holds it, used as the
// starting point of the update tests below.
func storedStore(auth crd.ESOVaultAuth) *crd.ESOSecretStore {
	store := &crd.ESOSecretStore{}
	store.Name = "store"
	store.Namespace = "demo"
	store.ResourceVersion = "42"
	store.Spec.Provider.Vault = &crd.ESOVaultProvider{
		Server:  "https://vault.example.com",
		Path:    "secret",
		Version: "v2",
		Auth:    auth,
	}
	return store
}

func kubernetesStored(serviceAccount string) *crd.ESOSecretStore {
	return storedStore(crd.ESOVaultAuth{Kubernetes: &crd.ESOKubernetesAuth{
		MountPath:         "kubernetes",
		Role:              "demo-role",
		ServiceAccountRef: &crd.ESOServiceAccountRef{Name: serviceAccount},
	}})
}

func tokenStored() *crd.ESOSecretStore {
	return storedStore(crd.ESOVaultAuth{TokenSecretRef: &crd.ESOTokenSecretRef{
		Name: "store-credentials",
		Key:  "token",
	}})
}

func updateRequest(auth *models.SecretStoreAuth) models.SecretStoreRequest {
	return models.SecretStoreRequest{
		Name:     "store",
		Provider: "vault",
		Vault:    &models.VaultConfig{Server: "https://vault.example.com", Path: "secret", Version: "v2"},
		Auth:     auth,
	}
}

// updated runs an update against a store the cluster already holds and returns
// the CR handed to the repository, which is what actually reaches the cluster.
func updated(t *testing.T, stored *crd.ESOSecretStore, req models.SecretStoreRequest, arrange func(*mocks.SecretStoreRepository)) (*crd.ESOSecretStore, error) {
	t.Helper()
	repo := &mocks.SecretStoreRepository{}
	repo.On("Get", mock.Anything, "demo", "store").Return(stored, nil)

	var sent *crd.ESOSecretStore
	repo.On("Update", mock.Anything, "demo", mock.Anything).Run(func(args mock.Arguments) {
		sent = args.Get(2).(*crd.ESOSecretStore)
	}).Return(nil).Maybe()
	if arrange != nil {
		arrange(repo)
	}

	svc := &DefaultSecretStoreService{repo: repo, platform: func(context.Context) *platformSecretStore { return sandboxStore }}
	_, err := svc.UpdateSecretStore(context.Background(), "demo", "store", req)
	return sent, err
}

// A store written before the account was pinned, of another account, is
// repointed at the platform's on its next update; another account named
// is refused.
func TestUpdatePinsTheServiceAccount(t *testing.T) {
	for _, sa := range []*string{nil, serviceAccountPtr("")} {
		sent, err := updated(t, kubernetesStored("vault-reader"), updateRequest(kubernetesAuth(sa)), nil)
		if err != nil {
			t.Fatalf("update failed: %v", err)
		}
		if ref := sent.Spec.Provider.Vault.Auth.Kubernetes.ServiceAccountRef; ref.Name != "okdp-secrets" || len(ref.Audiences) != 1 {
			t.Fatalf("serviceAccountRef %+v, want okdp-secrets of the platform's audience", ref)
		}
	}
	sent, err := updated(t, kubernetesStored("okdp-secrets"), updateRequest(kubernetesAuth(serviceAccountPtr("vault-reader"))), nil)
	if !IsValidationError(err) || sent != nil {
		t.Fatalf("another account: %v, updated %v", err, sent != nil)
	}
}

// Switching to token auth without a token would write a CR referencing a Secret
// that was never created: the store can never sync, and the API used to answer
// success all the same.
func TestUpdateRefusesTokenAuthWithoutAToken(t *testing.T) {
	_, err := updated(t, kubernetesStored("vault-reader"), updateRequest(&models.SecretStoreAuth{
		Type:   "token",
		Config: models.SecretAuthConfig{},
	}), nil)
	if err == nil {
		t.Fatal("switching to token auth with no token was accepted")
	}
}

// A store that already authenticates by token keeps it, so an edit that only
// renames the store does not force the operator to paste the token again.
func TestUpdateKeepsTheStoredTokenWhenTheFieldIsAbsent(t *testing.T) {
	repoCalls := &mocks.SecretStoreRepository{}
	_, err := updated(t, tokenStored(), updateRequest(&models.SecretStoreAuth{
		Type:   "token",
		Config: models.SecretAuthConfig{},
	}), func(r *mocks.SecretStoreRepository) { repoCalls = r })
	if err != nil {
		t.Fatalf("update failed: %v", err)
	}
	repoCalls.AssertNotCalled(t, "CreateOrUpdateSecret", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// Leaving token auth strands the credentials Secret: the CR no longer points at
// it, nothing in the console shows it, and the token stays valid in Vault.
func TestUpdateDropsTheTokenSecretWhenLeavingTokenAuth(t *testing.T) {
	var deleted string
	sent, err := updated(t, tokenStored(), updateRequest(kubernetesAuth(nil)),
		func(r *mocks.SecretStoreRepository) {
			r.On("DeleteSecret", mock.Anything, "demo", mock.Anything).Run(func(args mock.Arguments) {
				deleted = args.String(2)
			}).Return(nil)
		})
	if err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if sent.Spec.Provider.Vault.Auth.Kubernetes == nil {
		t.Fatal("the store did not switch to kubernetes auth")
	}
	if deleted != "store-credentials" {
		t.Fatalf("deleted secret is %q, want store-credentials", deleted)
	}
}

// A store that stays on token auth must keep its Secret, or every edit would
// break the store it just saved.
func TestUpdateKeepsTheTokenSecretWhenStayingOnTokenAuth(t *testing.T) {
	repoCalls := &mocks.SecretStoreRepository{}
	_, err := updated(t, tokenStored(), updateRequest(&models.SecretStoreAuth{
		Type:   "token",
		Config: models.SecretAuthConfig{Token: "s.nouveau"},
	}), func(r *mocks.SecretStoreRepository) {
		repoCalls = r
		r.On("CreateOrUpdateSecret", mock.Anything, "demo", "store-credentials", mock.Anything).Return(nil)
	})
	if err != nil {
		t.Fatalf("update failed: %v", err)
	}
	repoCalls.AssertNotCalled(t, "DeleteSecret", mock.Anything, mock.Anything, mock.Anything)
}

// A refusal caused by the request must reach the handler as a ValidationError,
// which is what turns it into a 400. Reported as a plain error it became a 500,
// so a console could only tell the user the server had broken when the fix was
// to fill in a field.
func TestUpdateRefusalsAreReportedAsBadRequests(t *testing.T) {
	cases := map[string]models.SecretStoreRequest{
		"no token on the switch to token auth": updateRequest(&models.SecretStoreAuth{
			Type:   "token",
			Config: models.SecretAuthConfig{},
		}),
		"no role on kubernetes auth": updateRequest(&models.SecretStoreAuth{
			Type:   "kubernetes",
			Config: models.SecretAuthConfig{MountPath: "kubernetes"},
		}),
		"unsupported auth type": updateRequest(&models.SecretStoreAuth{
			Type:   "appRole",
			Config: models.SecretAuthConfig{},
		}),
	}

	for name, req := range cases {
		_, err := updated(t, kubernetesStored("vault-reader"), req, nil)
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if !IsValidationError(err) {
			t.Fatalf("%s: refused as a server fault, not a bad request: %v", name, err)
		}
	}
}

// validateRequest lets the token be empty because an update reads that as
// "keep the stored one". A create has nothing to keep: the store was written
// with an empty Secret, could never authenticate, and the API answered 201.
func TestCreateRefusesTokenAuthWithoutAToken(t *testing.T) {
	repo := &mocks.SecretStoreRepository{}
	repo.On("CreateOrUpdateSecret", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	repo.On("Create", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	repo.On("Get", mock.Anything, mock.Anything, mock.Anything).Return(tokenStored(), nil).Maybe()
	svc := &DefaultSecretStoreService{repo: repo}

	_, err := svc.CreateSecretStore(context.Background(), "demo", models.SecretStoreRequest{
		Name:     "store",
		Provider: "vault",
		Vault:    &models.VaultConfig{Server: "https://vault.example.com", Path: "secret", Version: "v2"},
		Auth:     &models.SecretStoreAuth{Type: "token", Config: models.SecretAuthConfig{}},
	})
	if err == nil {
		t.Fatal("a token store was created with no token")
	}
	if !IsValidationError(err) {
		t.Fatalf("refused as a platform failure rather than a rejected input: %v", err)
	}
	repo.AssertNotCalled(t, "Create", mock.Anything, mock.Anything, mock.Anything)
}

// A mistyped ServiceAccount reached the API server, came back as a schema
// rejection the handler could not tell from a platform failure, and answered
// 500 where the fix was to correct a name.
func TestKubernetesAuthRefusesAnInvalidServiceAccount(t *testing.T) {
	for _, sa := range []string{"Mon_Compte", "compte invalide", "-debut", "MAJUSCULE"} {
		err := validateRequest(models.SecretStoreRequest{
			Name:     "store",
			Provider: "vault",
			Vault:    &models.VaultConfig{Server: "https://vault.example.com", Path: "secret", Version: "v2"},
			Auth:     kubernetesAuth(serviceAccountPtr(sa)),
		})
		if !IsValidationError(err) {
			t.Fatalf("service account %q was accepted or misreported: %v", sa, err)
		}
	}
	// A valid one, and an emptied one asking for the default, both pass.
	for _, sa := range []*string{serviceAccountPtr("vault-reader"), serviceAccountPtr(""), nil} {
		if err := validateRequest(models.SecretStoreRequest{
			Name:     "store",
			Provider: "vault",
			Vault:    &models.VaultConfig{Server: "https://vault.example.com", Path: "secret", Version: "v2"},
			Auth:     kubernetesAuth(sa),
		}); err != nil {
			t.Fatalf("a legitimate service account was rejected: %v", err)
		}
	}
}
