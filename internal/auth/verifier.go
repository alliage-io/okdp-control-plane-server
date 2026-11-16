// Package auth checks the bearer token of a request against the platform's
// issuer. It knows nothing about HTTP, so the verification is testable without
// a server.
package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Bounds the discovery call at startup and each later key refresh.
const issuerTimeout = 30 * time.Second

// Verifier validates a bearer token and returns the identity it represents.
type Verifier interface {
	Verify(ctx context.Context, rawToken string) (*Identity, error)
}

// Identity represents the user identified by a verified token.
type Identity struct {
	Subject  string
	Username string
	// Name is the display name (OIDC "name" claim, Keycloak "profile" scope).
	Name string
	// Email is the "email" claim (Keycloak "email" scope); empty when absent.
	Email string
	// Groups are the groups of the caller, the token's groups claim
	// (Config.GroupsClaim): the names of the platform's groups as the
	// identity provider gives them, Keycloak's group names (its groups
	// mapper's "Full group path" off), those of platform.yaml
	// global.okdp.access.admins and of each project.yaml access.
	Groups []string
}

// DefaultGroupsClaim is the claim of a token that holds the caller's
// groups when Config.GroupsClaim names none (OIDC_GROUPS_CLAIM).
const DefaultGroupsClaim = "groups"

// Config describes the identity provider to trust.
type Config struct {
	Issuer string
	// InsecureSkipVerify takes the issuer's certificate on trust
	// (global.okdp.oidc.insecureSkipVerify): discouraged, RootCAs given the
	// platform's CA instead.
	InsecureSkipVerify bool
	// RootCAs verify the issuer's certificate: the system's and the
	// platform's CA bundle (OKDP_CA_FILE); nil, the system's.
	RootCAs *x509.CertPool
	ClientID           string
	// GroupsClaim is the claim holding the caller's groups, a list of
	// strings (a single string is one group); a dotted name is a claim
	// nested in objects (Keycloak's mapper writes "a.b" so).
	// DefaultGroupsClaim when empty.
	GroupsClaim string
}

type oidcVerifier struct {
	verifier    *oidc.IDTokenVerifier
	clientID    string
	groupsClaim string
}

// idClaims contains the claims needed by Verify to identify the client and user.
// RFC 9068 uses client_id, while Keycloak uses azp.
type idClaims struct {
	Azp               string `json:"azp"`
	ClientID          string `json:"client_id"`
	PreferredUsername string `json:"preferred_username"`
	Name              string `json:"name"`
	Email             string `json:"email"`
}

// NewVerifier resolves the issuer's discovery document.
func NewVerifier(ctx context.Context, cfg Config) (Verifier, error) {
	if cfg.ClientID == "" {
		return nil, errors.New("no OIDC client id: a token would authenticate without proving it was issued for this API")
	}

	// go-oidc otherwise falls back to http.DefaultClient, which has no timeout.
	client := &http.Client{Timeout: issuerTimeout}
	if cfg.InsecureSkipVerify || cfg.RootCAs != nil {
		// Cloned, not built from scratch: a bare Transport has no Proxy, and
		// the chart passes the egress proxy the pod must go through.
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{RootCAs: cfg.RootCAs, MinVersion: tls.VersionTLS12}
		if cfg.InsecureSkipVerify {
			transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- opt-in, discouraged
		}
		client.Transport = transport
	}

	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("could not reach the OIDC issuer %q: %w", cfg.Issuer, err)
	}

	// SkipClientIDCheck: Verify checks aud/azp/client_id itself, below.
	groupsClaim := strings.TrimSpace(cfg.GroupsClaim)
	if groupsClaim == "" {
		groupsClaim = DefaultGroupsClaim
	}
	return &oidcVerifier{
		verifier:    provider.Verifier(&oidc.Config{SkipClientIDCheck: true}),
		clientID:    cfg.ClientID,
		groupsClaim: groupsClaim,
	}, nil
}

func (v *oidcVerifier) Verify(ctx context.Context, rawToken string) (*Identity, error) {
	token, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return nil, err
	}

	var claims idClaims
	if err := token.Claims(&claims); err != nil {
		return nil, fmt.Errorf("could not read the token claims: %w", err)
	}

	named := claims.ClientID == v.clientID || claims.Azp == v.clientID
	for _, aud := range token.Audience {
		named = named || aud == v.clientID
	}
	if !named {
		return nil, errors.New("the token was not issued for this client")
	}

	var all map[string]any
	if err := token.Claims(&all); err != nil {
		return nil, fmt.Errorf("could not read the token claims: %w", err)
	}

	username := claims.PreferredUsername
	if username == "" {
		username = claims.Email
	}
	return &Identity{Subject: token.Subject, Username: username, Name: claims.Name, Email: claims.Email, Groups: groupsOf(all, v.groupsClaim)}, nil
}

// groupsOf returns the groups of the claim name of claims, a dotted name
// one nested in objects: a list's strings, or a string as one group; none
// otherwise.
func groupsOf(claims map[string]any, name string) []string {
	var v any = claims
	for part := range strings.SplitSeq(name, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[part]
	}
	switch g := v.(type) {
	case string:
		return []string{g}
	case []any:
		groups := make([]string, 0, len(g))
		for _, item := range g {
			if s, ok := item.(string); ok {
				groups = append(groups, s)
			}
		}
		return groups
	}
	return nil
}

// ResolveIssuer prefers the environment override, then what the platform
// Context declares. Empty is an error: no issuer means no way to tell a real
// token from a forged one.
func ResolveIssuer(ctx context.Context, override string, fromContext func(context.Context) (string, error)) (string, error) {
	if override != "" {
		return override, nil
	}

	issuer, err := fromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("could not read the OIDC issuer from the platform Context: %w", err)
	}
	if issuer == "" {
		return "", errors.New("no OIDC issuer: the platform Context declares none and OIDC_ISSUER is unset")
	}
	return issuer, nil
}

// ResolveClientID mirrors ResolveIssuer.
func ResolveClientID(ctx context.Context, override string, fromContext func(context.Context) (string, error)) (string, error) {
	if override != "" {
		return override, nil
	}

	clientID, err := fromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("could not read the OIDC client id from the platform Context: %w", err)
	}
	if clientID == "" {
		return "", errors.New("no OIDC client id: the platform Context declares none and OIDC_CLIENT_ID is unset")
	}
	return clientID, nil
}
