package auth

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// RFC 8693 identifiers.
const (
	grantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	tokenTypeAccess    = "urn:ietf:params:oauth:token-type:access_token"
)

// exchangeBodyLimit bounds what is read of the token endpoint's answer.
const exchangeBodyLimit = 1 << 20

// TokenExchanger trades the bearer token a caller of the API presented
// for one of the engine's audience: what the server forwards instead of
// the caller's own token, which the API would accept back.
type TokenExchanger interface {
	// Exchange returns a token for the engine, of the same caller, for
	// subjectToken. The errors name no token.
	Exchange(ctx context.Context, subjectToken string) (string, error)
}

// ExchangeConfig describes the identity provider's token exchange.
type ExchangeConfig struct {
	// Issuer, InsecureSkipVerify, RootCAs: as Config.
	Issuer             string
	InsecureSkipVerify bool
	RootCAs            *x509.CertPool
	// ConsoleClientID is the client the API verifies tokens against
	// (Config.ClientID): an exchanged token naming it is refused.
	ConsoleClientID string
	// ClientID and ClientSecret identify the confidential client that
	// exchanges the tokens. Never the console's client, whose tokens the
	// API accepts.
	ClientID     string
	ClientSecret string
	// Audience is the audience of the exchanged token: the engine's
	// client (http-server.authentication.oauth2.client-id, or an
	// additional-audience, of each Trino).
	Audience string
}

type oidcExchanger struct {
	tokenURL string
	client   *http.Client
	cfg      ExchangeConfig
}

// NewTokenExchanger resolves the issuer's discovery document, which
// names the token endpoint exchanged against. A client the same as the
// console's is refused: the exchanged token would be good at /api.
func NewTokenExchanger(ctx context.Context, cfg ExchangeConfig) (TokenExchanger, error) {
	if cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.Audience == "" {
		return nil, errors.New("a token exchange needs a client, its secret and an audience")
	}
	if cfg.ClientID == cfg.ConsoleClientID {
		return nil, fmt.Errorf("the token exchange client %q is the console's: the exchanged token would be accepted at /api", cfg.ClientID)
	}
	client := issuerClient(cfg.InsecureSkipVerify, cfg.RootCAs)
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("could not reach the OIDC issuer %q: %w", cfg.Issuer, err)
	}
	tokenURL := provider.Endpoint().TokenURL
	if tokenURL == "" {
		return nil, fmt.Errorf("the OIDC issuer %q publishes no token endpoint", cfg.Issuer)
	}
	return &oidcExchanger{tokenURL: tokenURL, client: client, cfg: cfg}, nil
}

func (e *oidcExchanger) Exchange(ctx context.Context, subjectToken string) (string, error) {
	form := url.Values{
		"grant_type":           {grantTokenExchange},
		"subject_token":        {subjectToken},
		"subject_token_type":   {tokenTypeAccess},
		"requested_token_type": {tokenTypeAccess},
		"audience":             {e.cfg.Audience},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// RFC 6749 2.3.1: the client's credentials are form-urlencoded in
	// the header's user and password.
	req.SetBasicAuth(url.QueryEscape(e.cfg.ClientID), url.QueryEscape(e.cfg.ClientSecret))

	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("the token exchange failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, exchangeBodyLimit))
	if err != nil {
		return "", fmt.Errorf("reading the token exchange's answer: %w", err)
	}
	var answer struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &answer)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the token exchange was refused (%d): %s %s", resp.StatusCode, answer.Error, answer.Description)
	}
	if answer.AccessToken == "" {
		return "", errors.New("the token exchange gave no access token")
	}
	if err := refuseReplayable(answer.AccessToken, e.cfg.ConsoleClientID); err != nil {
		return "", err
	}
	return answer.AccessToken, nil
}

// refuseReplayable: an exchanged token is forwarded only when its claims
// leave it refused at /api, where a token is taken by aud, azp or
// client_id (Verify). The token comes from the issuer over TLS: only
// its claims are read here, its signature is the engine's to check.
func refuseReplayable(token, console string) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("the exchanged token is no JWT: nothing shows the API would refuse it")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return errors.New("the exchanged token does not parse")
	}
	var claims struct {
		Azp      string `json:"azp"`
		ClientID string `json:"client_id"`
		Aud      any    `json:"aud"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return errors.New("the exchanged token does not parse")
	}
	audience := []string{}
	switch aud := claims.Aud.(type) {
	case string:
		audience = append(audience, aud)
	case []any:
		for _, a := range aud {
			if s, ok := a.(string); ok {
				audience = append(audience, s)
			}
		}
	}
	if claims.Azp == console || claims.ClientID == console || slices.Contains(audience, console) {
		return fmt.Errorf("the exchanged token names the console's client %q: it would be accepted at /api", console)
	}
	return nil
}
