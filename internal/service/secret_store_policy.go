package service

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	compiler "github.com/okdp/okdp-compiler"
	"github.com/sirupsen/logrus"
)

// platformSecretStore is the platform's secret store as the compile writes
// it in every project's namespace (compiler.Result.SecretStore, of
// global.okdp.secrets): the Vault server of its provider, the
// ServiceAccount of the namespace it authenticates with, which the
// compiled manifests of each project create, and the audiences of the
// token External Secrets requests for it.
type platformSecretStore struct {
	server         string
	serviceAccount string
	audiences      []string
}

// platformStore is the platform's secret store: a test's, else the
// compile's (compiledStore).
func (s *DefaultSecretStoreService) platformStore(ctx context.Context) *platformSecretStore {
	if s.platform != nil {
		return s.platform(ctx)
	}
	return s.compiledStore(ctx)
}

// compiledStore reads the platform's secret store off the compile of the
// tree: the SecretStore of the first instance that compiles, the same for
// every namespace (the platform's settings). nil when the platform names
// no provider (a ClusterSecretStore), when no instance compiles, or
// without a tree.
func (s *DefaultSecretStoreService) compiledStore(ctx context.Context) *platformSecretStore {
	if s.tree == nil {
		return nil
	}
	t, err := s.tree(ctx)
	if err != nil {
		logrus.WithError(err).Warn("The platform's secret store is unknown: the tree does not compile")
		return nil
	}
	for _, inst := range t.Instances {
		res := inst.Outcome.Result
		if res == nil {
			continue
		}
		if res.SecretStore == nil {
			return nil
		}
		return storeOf(res)
	}
	return nil
}

// storeOf reads the SecretStore of a compiled result: its provider is the
// platform's text, External Secrets' spec.provider, verbatim.
func storeOf(res *compiler.Result) *platformSecretStore {
	vault, _ := res.SecretStore.Provider["vault"].(map[string]any)
	server, _ := vault["server"].(string)
	store := &platformSecretStore{server: server, serviceAccount: res.SecretStore.ServiceAccount}
	auth, _ := vault["auth"].(map[string]any)
	k8s, _ := auth["kubernetes"].(map[string]any)
	ref, _ := k8s["serviceAccountRef"].(map[string]any)
	audiences, _ := ref["audiences"].([]any)
	for _, a := range audiences {
		if a, ok := a.(string); ok && a != "" {
			store.audiences = append(store.audiences, a)
		}
	}
	return store
}

// normalizeVaultServer is the form two Vault URLs are compared in: the
// scheme and the host lowercased, the scheme's default port and a
// trailing slash dropped; an error for a URL that is not http(s), or that
// carries credentials, a query or a fragment.
func normalizeVaultServer(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("not a URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", fmt.Errorf("not an http(s) URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.Hostname() == "" {
		return "", fmt.Errorf("a URL of a host and a path only")
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && port != map[string]string{"https": "443", "http": "80"}[scheme] {
		host += ":" + port
	}
	return scheme + "://" + host + strings.TrimRight(u.EscapedPath(), "/"), nil
}

// allowedServer refuses a Vault server that is neither the platform's
// secret store's (platform, nil when none) nor one of the servers the
// control plane's settings allow (SECRET_STORE_ALLOWED_SERVERS): a store
// of a project sends its credentials, a token of its namespace's
// ServiceAccount, to its server, and a connection test the caller's
// token.
func (s *DefaultSecretStoreService) allowedServer(server string, platform *platformSecretStore) error {
	want, err := normalizeVaultServer(server)
	if err != nil {
		return invalid("vault.server %q: %v", server, err)
	}
	allowed := slices.Clone(s.allowedServers)
	if platform != nil && platform.server != "" {
		allowed = append(allowed, platform.server)
	}
	for _, a := range allowed {
		if n, err := normalizeVaultServer(a); err == nil && n == want {
			return nil
		}
	}
	return invalid("vault.server %q is not an allowed Vault server: the platform's secret store's (global.okdp.secrets.provider.vault.server) or one of the control plane's secretStores.allowedServers (SECRET_STORE_ALLOWED_SERVERS)", server)
}

// kubernetesIdentity is what a store of kubernetes auth authenticates as
// in namespace: the ServiceAccount of the platform's secret store, which
// the compiled manifests of the project create there, and the
// platform's audiences; the request may name that account, no other.
func kubernetesIdentity(requested *string, platform *platformSecretStore) (*platformSecretStore, error) {
	if platform == nil || platform.serviceAccount == "" {
		return nil, invalid("kubernetes auth authenticates as the ServiceAccount of the platform's secret store in the project's namespace: the platform names no provider of its secret store (global.okdp.secrets.provider), or no instance of the tree compiles yet")
	}
	if requested != nil && *requested != "" && *requested != platform.serviceAccount {
		return nil, invalid("auth.config.serviceAccount %q: a store authenticates as %s, the ServiceAccount of the platform's secret store in the namespace (global.okdp.secrets.serviceAccount), and no other", *requested, platform.serviceAccount)
	}
	if len(platform.audiences) == 0 {
		return nil, invalid("the platform's secret store names no audiences (global.okdp.secrets.provider.vault.auth.kubernetes.serviceAccountRef.audiences): the token sent to the Vault server would be one of the API server's audience")
	}
	return platform, nil
}
