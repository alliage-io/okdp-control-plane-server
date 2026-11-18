package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jwtOf is a JWT of the claims, unsigned: only its claims are read here.
func jwtOf(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// exchangerFor is a token exchange at a provider whose token endpoint
// answers with handler.
func exchangerFor(t *testing.T, audience string, handler http.HandlerFunc) TokenExchanger {
	t.Helper()
	idp := newFakeIDP(t)
	idp.server.Config.Handler.(*http.ServeMux).HandleFunc("/token", handler)
	exchanger, err := NewTokenExchanger(context.Background(), ExchangeConfig{
		Issuer: idp.server.URL, ConsoleClientID: "okdp-ui",
		ClientID: "okdp-control-plane", ClientSecret: "s3cret", Audience: audience,
	})
	require.NoError(t, err)
	return exchanger
}

// RFC 8693: the caller's token is the subject, the audience the engine's,
// the client authenticated as a confidential client.
func TestExchangeAsksTheProviderForTheEnginesAudience(t *testing.T) {
	var got http.Request
	exchanger := exchangerFor(t, "trino", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		got = *r
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": jwtOf(t, map[string]any{"aud": "trino", "azp": "okdp-control-plane"})})
	})

	token, err := exchanger.Exchange(context.Background(), "caller-token")

	require.NoError(t, err)
	assert.NotEqual(t, "caller-token", token)
	assert.Equal(t, grantTokenExchange, got.PostForm.Get("grant_type"))
	assert.Equal(t, "caller-token", got.PostForm.Get("subject_token"))
	assert.Equal(t, tokenTypeAccess, got.PostForm.Get("subject_token_type"))
	assert.Equal(t, "trino", got.PostForm.Get("audience"))
	user, pass, ok := got.BasicAuth()
	assert.True(t, ok)
	assert.Equal(t, "okdp-control-plane", user)
	assert.Equal(t, "s3cret", pass)
}

// A token the API would accept is not forwarded: a provider misconfigured
// to give the console's client in aud, azp or client_id, an opaque token.
func TestExchangeRefusesAReplayableToken(t *testing.T) {
	for name, token := range map[string]string{
		"aud":       jwtOf(t, map[string]any{"aud": []string{"trino", "okdp-ui"}, "azp": "okdp-control-plane"}),
		"azp":       jwtOf(t, map[string]any{"aud": "trino", "azp": "okdp-ui"}),
		"client_id": jwtOf(t, map[string]any{"aud": "trino", "client_id": "okdp-ui"}),
		"opaque":    "not-a-jwt",
	} {
		t.Run(name, func(t *testing.T) {
			exchanger := exchangerFor(t, "trino", func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token})
			})
			_, err := exchanger.Exchange(context.Background(), "caller-token")
			require.Error(t, err)
		})
	}
}

// The token exchange refused: the provider's reason, no token in the error.
func TestExchangeRefusalIsAnError(t *testing.T) {
	exchanger := exchangerFor(t, "trino", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_request", "error_description": "client not within audience"})
	})
	_, err := exchanger.Exchange(context.Background(), "caller-token")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid_request")
	assert.False(t, strings.Contains(err.Error(), "caller-token"))
}

// The exchange client must differ from the console's: its tokens would be
// accepted at /api.
func TestAnExchangeClientTheSameAsTheConsolesIsRefused(t *testing.T) {
	idp := newFakeIDP(t)
	_, err := NewTokenExchanger(context.Background(), ExchangeConfig{
		Issuer: idp.server.URL, ConsoleClientID: "okdp-ui",
		ClientID: "okdp-ui", ClientSecret: "s", Audience: "trino",
	})
	require.Error(t, err)
}

// A token exchanged for the engine, as Keycloak gives it, is no token of
// /api: not named for the console's client, its signature good, refused.
func TestAnExchangedTokenIsRefusedAtTheAPI(t *testing.T) {
	idp := newFakeIDP(t)
	verifier, err := NewVerifier(context.Background(), Config{Issuer: idp.server.URL, ClientID: "okdp-ui"})
	require.NoError(t, err)

	exchanged := idp.claims()
	exchanged["aud"] = "trino"
	exchanged["azp"] = "okdp-control-plane"

	_, err = verifier.Verify(context.Background(), idp.mint(t, exchanged))
	assert.Error(t, err)
}
