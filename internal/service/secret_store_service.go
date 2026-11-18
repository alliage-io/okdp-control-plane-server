package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	compiler "github.com/okdp/okdp-compiler"

	"github.com/okdp/okdp-control-plane-server/internal/apperr"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/netguard"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

type SecretStoreService interface {
	// Available reports whether the external-secrets CRDs are installed.
	Available(ctx context.Context) bool

	ListSecretStores(ctx context.Context, namespace string) ([]models.SecretStoreResponse, error)
	CreateSecretStore(ctx context.Context, namespace string, req models.SecretStoreRequest) (*models.SecretStoreResponse, error)
	UpdateSecretStore(ctx context.Context, namespace, name string, req models.SecretStoreRequest) (*models.SecretStoreResponse, error)
	DeleteSecretStore(ctx context.Context, namespace, name string) error
	TestConnection(ctx context.Context, req models.SecretStoreRequest) error
	GetSecretStoreStatus(ctx context.Context, namespace, name string) (*models.SecretStoreStatusResponse, error)
}

type DefaultSecretStoreService struct {
	repo repository.SecretStoreRepository
	// tree is the compile of the tree: the platform's secret store, its
	// server, the ServiceAccount of each namespace and its audiences
	// (platformStore); nil, none.
	tree func(ctx context.Context) (*compiler.Tree, error)
	// platform, when set, is the platform's secret store instead (tests).
	platform func(ctx context.Context) *platformSecretStore
	// allowedServers are the Vault servers a store may name beside the
	// platform's (SECRET_STORE_ALLOWED_SERVERS).
	allowedServers []string
	// guard is the network a connection test reaches (netguard.Default
	// when nil): never loopback, link-local or the API server.
	guard *netguard.Guard
	// roots verify the certificate of a tested server: the system's and
	// the platform's CA bundle (OKDP_CA_FILE); nil, the system's.
	roots *x509.CertPool
}

func (s *DefaultSecretStoreService) Available(ctx context.Context) bool {
	return s.repo.Available(ctx)
}

// NewDefaultSecretStoreService manages the SecretStores of the projects'
// namespaces: tree is the compile of the tree, the platform's secret
// store read from it, allowedServers the Vault servers allowed beside its
// own.
func NewDefaultSecretStoreService(repo repository.SecretStoreRepository, tree func(ctx context.Context) (*compiler.Tree, error), allowedServers []string) *DefaultSecretStoreService {
	return &DefaultSecretStoreService{repo: repo, tree: tree, allowedServers: allowedServers}
}

// SetNetworkGuard sets the network policy of the connection tests: the SQL
// editor's.
func (s *DefaultSecretStoreService) SetNetworkGuard(guard *netguard.Guard) { s.guard = guard }

// SetRootCAs sets the certificates a tested server is verified with, the
// system's and the platform's CA bundle, and the request's caBundle.
func (s *DefaultSecretStoreService) SetRootCAs(roots *x509.CertPool) { s.roots = roots }

// checkKubernetesAuth pins a store of kubernetes auth to the platform's
// secret store: its server allowed (allowedServer), its ServiceAccount
// and audiences the platform's (kubernetesIdentity). A store of token auth
// sends the caller's own token, to the server the caller names.
func (s *DefaultSecretStoreService) checkKubernetesAuth(ctx context.Context, req models.SecretStoreRequest) (*platformSecretStore, error) {
	if req.Auth.Type != "kubernetes" {
		return nil, nil
	}
	platform := s.platformStore(ctx)
	identity, err := kubernetesIdentity(req.Auth.Config.ServiceAccount, platform)
	if err != nil {
		return nil, err
	}
	if err := s.allowedServer(req.Vault.Server, platform); err != nil {
		return nil, err
	}
	return identity, nil
}

func (s *DefaultSecretStoreService) ListSecretStores(ctx context.Context, namespace string) ([]models.SecretStoreResponse, error) {
	stores, err := s.repo.List(ctx, namespace)
	if err != nil {
		return nil, err
	}

	// Never nil: no store answers [] in JSON, not null.
	result := make([]models.SecretStoreResponse, 0, len(stores))
	for i := range stores {
		result = append(result, s.toResponse(&stores[i], namespace))
	}
	return result, nil
}

func (s *DefaultSecretStoreService) CreateSecretStore(ctx context.Context, namespace string, req models.SecretStoreRequest) (*models.SecretStoreResponse, error) {
	if err := validateRequest(req); err != nil {
		return nil, err
	}
	identity, err := s.checkKubernetesAuth(ctx, req)
	if err != nil {
		return nil, err
	}

	credSecretName := req.Name + "-credentials"

	// validateRequest lets the token be empty because an update reads that
	// as "keep the stored one". On a create there is nothing to keep: an
	// empty token writes an empty Secret and the store can never
	// authenticate, while the API answers 201.
	if req.Auth.Type == "token" && req.Auth.Config.Token == "" {
		return nil, invalid("auth.config.token is required for token auth")
	}

	// The store first: a name already taken answers 409 before any
	// credentials or label change. Created, the Secret and the labels follow,
	// the store taken back if its Secret cannot be written.
	store := buildSecretStoreCRD(namespace, req, credSecretName, identity)
	if err := s.repo.Create(ctx, namespace, store); err != nil {
		return nil, err
	}

	if req.Auth.Type == "token" {
		secretData := map[string][]byte{"token": []byte(req.Auth.Config.Token)}
		if err := s.repo.CreateOrUpdateSecret(ctx, namespace, credSecretName, secretData); err != nil {
			rollback, cancel := compensationContext(ctx)
			defer cancel()
			if rollbackErr := s.repo.Delete(rollback, namespace, req.Name); rollbackErr != nil {
				slog.Warn("Failed to take back the store whose credentials could not be stored", "error", rollbackErr, "store", req.Name)
			}
			return nil, storeCredentialsError(err)
		}
	}

	if req.IsDefault {
		if err := s.repo.RemoveDefaultLabel(ctx, namespace, req.Name); err != nil {
			slog.Warn("Failed to clear previous default store label", "error", err)
		}
	}

	created, err := s.repo.Get(ctx, namespace, req.Name)
	if err != nil {
		return nil, err
	}

	resp := s.toResponse(created, namespace)
	return &resp, nil
}

func (s *DefaultSecretStoreService) UpdateSecretStore(ctx context.Context, namespace, name string, req models.SecretStoreRequest) (*models.SecretStoreResponse, error) {
	existing, err := s.repo.Get(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	req.Name = name
	if err := validateRequest(req); err != nil {
		return nil, err
	}
	identity, err := s.checkKubernetesAuth(ctx, req)
	if err != nil {
		return nil, err
	}

	credSecretName := name + "-credentials"

	// What the store authenticates with today. An omitted token can only mean
	// "keep the stored one" when there is a stored one.
	existingVault := existing.Spec.Provider.Vault
	usedTokenAuth := existingVault != nil && existingVault.Auth.TokenSecretRef != nil

	if req.Auth.Type == "token" {
		if req.Auth.Config.Token != "" {
			secretData := map[string][]byte{"token": []byte(req.Auth.Config.Token)}
			if err := s.repo.CreateOrUpdateSecret(ctx, namespace, credSecretName, secretData); err != nil {
				return nil, storeCredentialsError(err)
			}
		} else if !usedTokenAuth {
			// The CR would reference a Secret that was never written, so the
			// store could never sync while the API reported success.
			return nil, invalid("auth.config.token is required when switching to token auth")
		}
	}

	if req.IsDefault {
		if err := s.repo.RemoveDefaultLabel(ctx, namespace, ""); err != nil {
			slog.Warn("Failed to clear previous default store label", "error", err)
		}
	}

	store := buildSecretStoreCRD(namespace, req, credSecretName, identity)
	store.ResourceVersion = existing.ResourceVersion

	if err := s.repo.Update(ctx, namespace, store); err != nil {
		return nil, err
	}

	// Dropped only once the stored CR no longer points at it, so a failed
	// update never destroys a credential the store still uses. Left behind, the
	// Vault token would outlive the store's use of it: still valid in Vault,
	// no longer shown anywhere in the console.
	if usedTokenAuth && req.Auth.Type != "token" {
		if err := s.repo.DeleteSecret(ctx, namespace, credSecretName); err != nil && !errors.Is(err, apperr.ErrNotFound) {
			slog.Warn(fmt.Sprintf("Failed to delete unused credentials secret %s", credSecretName), "error", err)
		}
	}

	updated, err := s.repo.Get(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	resp := s.toResponse(updated, namespace)
	return &resp, nil
}

func (s *DefaultSecretStoreService) DeleteSecretStore(ctx context.Context, namespace, name string) error {
	_, err := s.repo.Get(ctx, namespace, name)
	if err != nil {
		return err
	}

	credSecretName := name + "-credentials"
	if err := s.repo.DeleteSecret(ctx, namespace, credSecretName); err != nil {
		if !errors.Is(err, apperr.ErrNotFound) {
			slog.Warn(fmt.Sprintf("Failed to delete credentials secret %s", credSecretName), "error", err)
		}
	}

	return s.repo.Delete(ctx, namespace, name)
}

// TestConnection checks a store's settings without writing anything: its
// server one a store may name (allowedServer), reached through the guard
// of the SQL editor (loopback, link-local and the API server refused);
// kubernetes auth checked as a write checks it, the caller's token of
// token auth looked up.
func (s *DefaultSecretStoreService) TestConnection(ctx context.Context, req models.SecretStoreRequest) error {
	if req.Vault == nil {
		return invalid("vault configuration is required")
	}
	if req.Vault.Server == "" {
		return invalid("vault.server is required")
	}
	if req.Auth == nil {
		return invalid("auth configuration is required")
	}

	if req.Auth.Type == "kubernetes" {
		_, err := s.checkKubernetesAuth(ctx, req)
		return err
	}
	if err := s.allowedServer(req.Vault.Server, s.platformStore(ctx)); err != nil {
		return err
	}

	if req.Auth.Config.Token == "" {
		return invalid("auth.config.token is required for token auth")
	}

	guard := s.guard
	if guard == nil {
		guard = netguard.Default()
	}
	return validateVaultToken(ctx, guard, s.roots, req.Vault.Server, req.Auth.Config.Token, req.Vault.CABundle)
}

// validateVaultToken calls GET /v1/auth/token/lookup-self to verify that
// the token is valid. sys/health only checks network connectivity -- a bad
// token still gets "Ready" from ESO.
//
// The method matters: Vault's default policy grants "read" on this path, which
// maps to GET. A POST needs the "update" capability, which no least-privilege
// token carries, so only a root token would have passed.
//
// The request dials through guard, follows no redirect (the token would
// go to the server a redirect names) and goes through no proxy; a failure
// to connect is reported without the cause, which is logged.
func validateVaultToken(ctx context.Context, guard *netguard.Guard, roots *x509.CertPool, server, token, caBundle string) error {
	tlsConfig := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}

	if caBundle != "" {
		// The store's caBundle, beside what the server trusts.
		pool := x509.NewCertPool()
		if roots != nil {
			pool = roots.Clone()
		}
		if !pool.AppendCertsFromPEM([]byte(caBundle)) {
			return invalid("vault.caBundle: invalid CA bundle")
		}
		tlsConfig.RootCAs = pool
	}

	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsConfig, DialContext: guard.Dialer(10 * time.Second).DialContext},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	url := strings.TrimRight(server, "/") + "/v1/auth/token/lookup-self"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to build request: %w", err)
	}
	httpReq.Header.Set("X-Vault-Token", token)

	resp, err := client.Do(httpReq)
	if errors.Is(err, netguard.ErrRefused) {
		return invalid("vault.server %q: %v", server, netguard.ErrRefused)
	}
	if err != nil {
		slog.Info("A secret store's connection test failed", "error", err, "server", server)
		return fmt.Errorf("connection failed: the Vault server did not answer")
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusForbidden:
		return fmt.Errorf("invalid token: permission denied")
	default:
		return fmt.Errorf("vault returned status %d", resp.StatusCode)
	}
}

func (s *DefaultSecretStoreService) GetSecretStoreStatus(ctx context.Context, namespace, name string) (*models.SecretStoreStatusResponse, error) {
	store, err := s.repo.Get(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	conditions := mapConditions(store.Status.Conditions)
	status := deriveOverallStatus(store.Status.Conditions)

	var lastCheckedAt *string
	var lastError *string

	if len(store.Status.Conditions) > 0 {
		last := store.Status.Conditions[len(store.Status.Conditions)-1]
		if last.LastTransitionTime != "" {
			lastCheckedAt = &last.LastTransitionTime
		}
		if last.Status != "True" && last.Message != "" {
			lastError = &last.Message
		}
	}

	return &models.SecretStoreStatusResponse{
		Status:        status,
		Conditions:    conditions,
		LastCheckedAt: lastCheckedAt,
		LastError:     lastError,
	}, nil
}

// --- helpers ---

func validateRequest(req models.SecretStoreRequest) error {
	if req.Name == "" {
		return invalid("name is required")
	}
	if req.Provider != "vault" {
		return invalid("unsupported provider %q, only 'vault' is supported", req.Provider)
	}
	if req.Vault == nil {
		return invalid("vault configuration is required")
	}
	if req.Vault.Server == "" {
		return invalid("vault.server is required")
	}
	if req.Vault.Path == "" {
		return invalid("vault.path is required")
	}
	if req.Vault.Version != "v1" && req.Vault.Version != "v2" {
		return invalid("vault.version must be 'v1' or 'v2'")
	}
	if req.Auth == nil {
		return invalid("auth configuration is required")
	}
	switch req.Auth.Type {
	case "token":
		// Token can be empty on update (preserves existing)
	case "kubernetes":
		if req.Auth.Config.Role == "" {
			return invalid("auth.config.role is required for kubernetes auth")
		}
		// Left to the API server this came back as a schema rejection the
		// handler could not tell from a platform failure, so a mistyped
		// account answered 500.
		if sa := req.Auth.Config.ServiceAccount; sa != nil && *sa != "" {
			if msgs := validation.IsDNS1123Subdomain(*sa); len(msgs) > 0 {
				return invalid("auth.config.serviceAccount %q is not a valid ServiceAccount name: %s", *sa, msgs[0])
			}
		}
	default:
		return invalid("unsupported auth type %q, must be 'token' or 'kubernetes'", req.Auth.Type)
	}
	return nil
}

// buildSecretStoreCRD is the SecretStore of a request; identity is what a
// store of kubernetes auth authenticates as (checkKubernetesAuth).
func buildSecretStoreCRD(namespace string, req models.SecretStoreRequest, credSecretName string, identity *platformSecretStore) *crd.ESOSecretStore {
	store := &crd.ESOSecretStore{
		TypeMeta: metav1.TypeMeta{
			APIVersion: crd.SecretStoreAPIVersion,
			Kind:       crd.SecretStoreKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: namespace,
		},
		Spec: crd.ESOSecretStoreSpec{
			Provider: crd.ESOProvider{
				Vault: &crd.ESOVaultProvider{
					Server:   req.Vault.Server,
					Path:     req.Vault.Path,
					Version:  req.Vault.Version,
					CABundle: []byte(req.Vault.CABundle),
				},
			},
		},
	}

	switch req.Auth.Type {
	case "token":
		store.Spec.Provider.Vault.Auth.TokenSecretRef = &crd.ESOTokenSecretRef{
			Name: credSecretName,
			Key:  "token",
		}
	case "kubernetes":
		mountPath := req.Auth.Config.MountPath
		if mountPath == "" {
			mountPath = "kubernetes"
		}
		// The ServiceAccount of the platform's secret store, which the
		// compiled manifests create in the namespace, and the platform's
		// audiences: External Secrets requests a token of that account for
		// those audiences only, never one of the API server's.
		store.Spec.Provider.Vault.Auth.Kubernetes = &crd.ESOKubernetesAuth{
			MountPath: mountPath,
			Role:      req.Auth.Config.Role,
			ServiceAccountRef: &crd.ESOServiceAccountRef{
				Name:      identity.serviceAccount,
				Audiences: slices.Clone(identity.audiences),
			},
		}
	}

	if req.IsDefault {
		store.Labels = map[string]string{crd.LabelDefaultStore: "true"}
	}

	return store
}

func (s *DefaultSecretStoreService) toResponse(store *crd.ESOSecretStore, namespace string) models.SecretStoreResponse {
	resp := models.SecretStoreResponse{
		Name:      store.Name,
		Provider:  store.Spec.Provider.Name,
		Namespace: namespace,
		Status:    deriveOverallStatus(store.Status.Conditions),
		IsDefault: store.Labels[crd.LabelDefaultStore] == "true",
		CreatedAt: store.CreationTimestamp.Format(time.RFC3339),
	}

	if len(store.Status.Conditions) > 0 {
		last := store.Status.Conditions[len(store.Status.Conditions)-1]
		if last.LastTransitionTime != "" {
			resp.LastCheckedAt = &last.LastTransitionTime
		}
		if last.Status != "True" && last.Message != "" {
			resp.LastError = &last.Message
		}
	}

	if store.Spec.Provider.Vault != nil {
		v := store.Spec.Provider.Vault
		resp.Vault = &models.VaultConfig{
			Server:   v.Server,
			Path:     v.Path,
			Version:  v.Version,
			CABundle: string(v.CABundle),
		}

		authResp := &models.SecretStoreAuthResponse{
			Config: models.SecretAuthConfigSafe{},
		}
		if v.Auth.TokenSecretRef != nil {
			authResp.Type = "token"
		} else if v.Auth.Kubernetes != nil {
			authResp.Type = "kubernetes"
			mp := v.Auth.Kubernetes.MountPath
			authResp.Config.MountPath = &mp
			role := v.Auth.Kubernetes.Role
			authResp.Config.Role = &role
			// Read back the account the store authenticates as, otherwise a
			// caller cannot tell which identity the Vault role must bind.
			if ref := v.Auth.Kubernetes.ServiceAccountRef; ref != nil {
				sa := ref.Name
				authResp.Config.ServiceAccount = &sa
			}
		}
		resp.Auth = authResp
	}

	return resp
}

func mapConditions(conditions []crd.ESOCondition) []models.SecretStoreCondition {
	out := make([]models.SecretStoreCondition, 0, len(conditions))
	for _, c := range conditions {
		out = append(out, models.SecretStoreCondition{
			Type:               c.Type,
			Status:             c.Status,
			Reason:             c.Reason,
			Message:            c.Message,
			LastTransitionTime: c.LastTransitionTime,
		})
	}
	return out
}

func deriveOverallStatus(conditions []crd.ESOCondition) string {
	if len(conditions) == 0 {
		return "Unknown"
	}
	for _, c := range conditions {
		if c.Type == "Ready" {
			switch c.Status {
			case "True":
				return "Ready"
			case "False":
				return "Error"
			default:
				return "Pending"
			}
		}
	}
	return "Pending"
}
