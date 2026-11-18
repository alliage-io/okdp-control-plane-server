package repository

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/apperr"
	"github.com/okdp/okdp-control-plane-server/internal/config"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newFakeKeycloak starts an httptest server answering the token endpoint
// plus the routes registered on the returned mux, and returns a config
// pointing at it. tokenCalls counts token requests (for 401-retry tests).
func newFakeKeycloak(t *testing.T, register func(mux *http.ServeMux)) (*config.Config, *int) {
	t.Helper()
	cfg, calls, _ := newFakeKeycloakOn(t, httptest.NewServer, register)
	return cfg, calls
}

// newFakeKeycloakOn is a fake Keycloak served by start, and its server:
// over https, httptest.NewTLSServer, its certificate trusted by no system.
func newFakeKeycloakOn(t *testing.T, start func(http.Handler) *httptest.Server, register func(mux *http.ServeMux)) (*config.Config, *int, *httptest.Server) {
	t.Helper()

	tokenCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/realms/okdp/protocol/openid-connect/token", func(w http.ResponseWriter, r *http.Request) {
		tokenCalls++
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "test-token",
			"expires_in":   300,
		})
	})
	if register != nil {
		register(mux)
	}

	srv := start(mux)
	t.Cleanup(srv.Close)

	return &config.Config{
		KeycloakURL:          srv.URL,
		KeycloakRealm:        "okdp",
		KeycloakClientID:     "okdp-control-plane",
		KeycloakClientSecret: "secret",
	}, &tokenCalls, srv
}

// Keycloak over https, its certificate of a CA the system does not trust:
// reached with the platform's CA bundle (OKDP_CA_FILE), refused without.
func TestKeycloakIsVerifiedWithThePlatformsCA(t *testing.T) {
	cfg, _, srv := newFakeKeycloakOn(t, httptest.NewTLSServer, func(mux *http.ServeMux) {
		mux.HandleFunc("/admin/realms/okdp/groups", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]kcGroup{{ID: "g1", Name: "team-a"}})
		})
	})
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())

	_, err := NewKeycloakIdentityRepository(cfg, nil, nil).ListGroups(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "certificate")
	groups, err := NewKeycloakIdentityRepository(cfg, nil, roots).ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, groups, 1)
}

func TestKeycloakGetUserMapsAttributes(t *testing.T) {
	cfg, _ := newFakeKeycloak(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/admin/realms/okdp/users", func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "jdoe", r.URL.Query().Get("username"))
			_ = json.NewEncoder(w).Encode([]kcUser{{
				ID:        "u1",
				Username:  "jdoe",
				FirstName: "John",
				LastName:  "Doe",
				Email:     "jdoe@example.com",
				Enabled:   false,
				Attributes: map[string][]string{
					"uid":     {"1234"},
					"comment": {"a comment"},
				},
			}})
		})
	})

	repo := NewKeycloakIdentityRepository(cfg, nil, nil)
	user, err := repo.GetUser(context.Background(), "jdoe")

	require.NoError(t, err)
	assert.Equal(t, "jdoe", user.Username)
	assert.Equal(t, "John Doe", user.Name)
	assert.Equal(t, []string{"jdoe@example.com"}, user.Email)
	assert.Equal(t, 1234, user.UID)
	assert.Equal(t, "a comment", user.Comment)
	assert.True(t, user.Disabled)
}

func TestKeycloakGetUserNotFound(t *testing.T) {
	cfg, _ := newFakeKeycloak(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/admin/realms/okdp/users", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]kcUser{})
		})
	})

	repo := NewKeycloakIdentityRepository(cfg, nil, nil)
	_, err := repo.GetUser(context.Background(), "ghost")

	require.Error(t, err)
	assert.True(t, errors.Is(err, apperr.ErrNotFound), "expected a NotFound error, got %v", err)
}

func TestKeycloakCreateUserSetsPassword(t *testing.T) {
	var createdUser kcUser
	var setCredential kcCredential

	cfg, _ := newFakeKeycloak(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/admin/realms/okdp/users", func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost:
				require.NoError(t, json.NewDecoder(r.Body).Decode(&createdUser))
				w.WriteHeader(http.StatusCreated)
			case http.MethodGet:
				_ = json.NewEncoder(w).Encode([]kcUser{{ID: "u1", Username: "jdoe"}})
			}
		})
		mux.HandleFunc("/admin/realms/okdp/users/u1/reset-password", func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPut, r.Method)
			require.NoError(t, json.NewDecoder(r.Body).Decode(&setCredential))
			w.WriteHeader(http.StatusNoContent)
		})
	})

	repo := NewKeycloakIdentityRepository(cfg, nil, nil)
	err := repo.CreateUser(context.Background(), &models.User{
		Username: "jdoe",
		Name:     "John Doe",
		Email:    []string{"jdoe@example.com"},
		Password: "s3cret",
	})

	require.NoError(t, err)
	assert.Equal(t, "jdoe", createdUser.Username)
	assert.Equal(t, "John", createdUser.FirstName)
	assert.Equal(t, "Doe", createdUser.LastName)
	assert.Equal(t, "jdoe@example.com", createdUser.Email)
	assert.True(t, createdUser.Enabled)
	assert.Equal(t, "password", setCredential.Type)
	assert.Equal(t, "s3cret", setCredential.Value)
	assert.False(t, setCredential.Temporary)
}

func TestKeycloakListGroupBindingsForUser(t *testing.T) {
	cfg, _ := newFakeKeycloak(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/admin/realms/okdp/users", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]kcUser{{ID: "u1", Username: "jdoe"}})
		})
		mux.HandleFunc("/admin/realms/okdp/users/u1/groups", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]kcGroup{{ID: "g1", Name: "team-a"}, {ID: "g2", Name: "team-b"}})
		})
	})

	repo := NewKeycloakIdentityRepository(cfg, nil, nil)
	bindings, err := repo.ListGroupBindings(context.Background(), "jdoe")

	require.NoError(t, err)
	require.Len(t, bindings, 2)
	assert.Equal(t, "jdoe", bindings[0].User)
	assert.Equal(t, "team-a", bindings[0].Group)
	assert.Equal(t, "team-b", bindings[1].Group)
}

// UpdateUser GETs the representation, writes the console's fields and
// owned attributes over it and PUTs it whole: the attributes and fields of
// LDAP or an administrator survive.
func TestKeycloakUpdateUserKeepsAttributesItDoesNotOwn(t *testing.T) {
	var put map[string]json.RawMessage
	cfg, _ := newFakeKeycloak(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/admin/realms/okdp/users", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]kcUser{{ID: "u1", Username: "jdoe"}})
		})
		mux.HandleFunc("/admin/realms/okdp/users/u1", func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"id":               "u1",
					"username":         "jdoe",
					"firstName":        "Old",
					"lastName":         "Name",
					"email":            "old@example.com",
					"enabled":          true,
					"emailVerified":    true,
					"federationLink":   "ldap-1",
					"createdTimestamp": int64(1730000000123),
					"requiredActions":  []string{"CONFIGURE_TOTP"},
					"attributes": map[string][]string{
						"LDAP_ID":    {"abc-123"},
						"department": {"R&D"},
						"comment":    {"old comment"},
						"uid":        {"1000"},
					},
				})
			case http.MethodPut:
				require.NoError(t, json.NewDecoder(r.Body).Decode(&put))
				w.WriteHeader(http.StatusNoContent)
			}
		})
	})

	repo := NewKeycloakIdentityRepository(cfg, nil, nil)
	require.NoError(t, repo.UpdateUser(context.Background(), "jdoe", &models.User{
		Username: "jdoe",
		Name:     "John Doe",
		Email:    []string{"jdoe@example.com"},
		Disabled: true,
	}))

	require.NotNil(t, put)
	var u kcUser
	raw, err := json.Marshal(put)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &u))
	assert.Equal(t, "u1", u.ID)
	assert.Equal(t, "jdoe", u.Username)
	assert.Equal(t, "John", u.FirstName)
	assert.Equal(t, "Doe", u.LastName)
	assert.Equal(t, "jdoe@example.com", u.Email)
	assert.False(t, u.Enabled)
	assert.Equal(t, map[string][]string{
		"LDAP_ID":    {"abc-123"},
		"department": {"R&D"},
	}, u.Attributes, "the console's comment and uid cleared, the others kept")

	assert.JSONEq(t, `true`, string(put["emailVerified"]))
	assert.JSONEq(t, `"ldap-1"`, string(put["federationLink"]))
	assert.Equal(t, `1730000000123`, string(put["createdTimestamp"]), "a number sent back verbatim")
	assert.JSONEq(t, `["CONFIGURE_TOTP"]`, string(put["requiredActions"]))
}

// A uid the console cannot show (not a number) is not its own: kept.
func TestKeycloakUpdateUserSetsOwnedAttributes(t *testing.T) {
	var put kcUser
	cfg, _ := newFakeKeycloak(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/admin/realms/okdp/users", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]kcUser{{ID: "u1", Username: "jdoe"}})
		})
		mux.HandleFunc("/admin/realms/okdp/users/u1", func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				_ = json.NewEncoder(w).Encode(kcUser{ID: "u1", Username: "jdoe", Enabled: true,
					Attributes: map[string][]string{"uid": {"jdoe"}}})
			case http.MethodPut:
				require.NoError(t, json.NewDecoder(r.Body).Decode(&put))
				w.WriteHeader(http.StatusNoContent)
			}
		})
	})

	repo := NewKeycloakIdentityRepository(cfg, nil, nil)
	require.NoError(t, repo.UpdateUser(context.Background(), "jdoe", &models.User{Username: "jdoe", Comment: "new"}))
	assert.Equal(t, map[string][]string{"comment": {"new"}, "uid": {"jdoe"}}, put.Attributes)

	put = kcUser{}
	require.NoError(t, repo.UpdateUser(context.Background(), "jdoe", &models.User{Username: "jdoe", UID: 2001}))
	assert.Equal(t, map[string][]string{"uid": {"2001"}}, put.Attributes)
}

func TestKeycloakGroupCRUDUsesGroupIDs(t *testing.T) {
	var updated kcGroup
	deleted := false

	cfg, _ := newFakeKeycloak(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/admin/realms/okdp/groups", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]kcGroup{{
				ID:         "g1",
				Name:       "team-a",
				Attributes: map[string][]string{"comment": {"the team"}},
			}})
		})
		mux.HandleFunc("/admin/realms/okdp/groups/g1", func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"id":         "g1",
					"name":       "team-a",
					"attributes": map[string][]string{"comment": {"the team"}, "ldap-dn": {"cn=team-a"}},
				})
				return
			case http.MethodPut:
				require.NoError(t, json.NewDecoder(r.Body).Decode(&updated))
			case http.MethodDelete:
				deleted = true
			}
			w.WriteHeader(http.StatusNoContent)
		})
	})

	repo := NewKeycloakIdentityRepository(cfg, nil, nil)

	group, err := repo.GetGroup(context.Background(), "team-a")
	require.NoError(t, err)
	assert.Equal(t, "team-a", group.Name)
	assert.Equal(t, "the team", group.Comment)

	require.NoError(t, repo.UpdateGroup(context.Background(), "team-a", &models.Group{Name: "team-a", Comment: "updated"}))
	assert.Equal(t, "g1", updated.ID)
	assert.Equal(t, []string{"updated"}, updated.Attributes["comment"])
	assert.Equal(t, []string{"cn=team-a"}, updated.Attributes["ldap-dn"], "an attribute the console does not own is kept")

	require.NoError(t, repo.DeleteGroup(context.Background(), "team-a"))
	assert.True(t, deleted)
}

func TestKeycloakRetriesOnceOn401(t *testing.T) {
	adminCalls := 0
	cfg, tokenCalls := newFakeKeycloak(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/admin/realms/okdp/users", func(w http.ResponseWriter, r *http.Request) {
			adminCalls++
			if adminCalls == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode([]kcUser{})
		})
	})

	repo := NewKeycloakIdentityRepository(cfg, nil, nil)
	users, err := repo.ListUsers(context.Background())

	require.NoError(t, err)
	assert.Empty(t, users)
	assert.Equal(t, 2, adminCalls, "the 401 should be retried once")
	assert.Equal(t, 2, *tokenCalls, "a fresh token should be fetched for the retry")
}

// Without a client secret there is nothing to authenticate with: the API is
// reported absent rather than failing on every call.
func TestKeycloakUnavailableWithoutCredentials(t *testing.T) {
	cfg, _ := newFakeKeycloak(t, nil)
	cfg.KeycloakClientSecret = ""

	repo := NewKeycloakIdentityRepository(cfg, nil, nil)

	assert.False(t, repo.Available(context.Background()))
	_, err := repo.ListUsers(context.Background())
	assert.ErrorIs(t, err, errKeycloakNotConfigured)
}

// KEYCLOAK_URL and KEYCLOAK_REALM unset: the realm is the platform issuer's,
// <url>/realms/<realm>, so the chart only has to provide the credentials.
func TestKeycloakRealmFollowsThePlatformIssuer(t *testing.T) {
	cfg, _ := newFakeKeycloak(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/admin/realms/okdp/groups", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]kcGroup{{ID: "g1", Name: "team-a"}})
		})
	})
	base := cfg.KeycloakURL
	cfg.KeycloakURL, cfg.KeycloakRealm = "", ""

	repo := NewKeycloakIdentityRepository(cfg, func(context.Context) (string, bool, error) {
		return base + "/realms/okdp/", false, nil
	}, nil)

	require.True(t, repo.Available(context.Background()))
	groups, err := repo.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, groups, 1)
	assert.Equal(t, "team-a", groups[0].Name)
}

// An issuer that is not a Keycloak realm cannot locate the Admin API.
func TestKeycloakUnavailableWhenTheIssuerIsNotARealm(t *testing.T) {
	cfg, _ := newFakeKeycloak(t, nil)
	cfg.KeycloakURL, cfg.KeycloakRealm = "", ""

	for _, issuer := range []string{"", "https://idp.example", "https://idp.example/realms/"} {
		repo := NewKeycloakIdentityRepository(cfg, func(context.Context) (string, bool, error) {
			return issuer, false, nil
		}, nil)
		assert.False(t, repo.Available(context.Background()), "issuer %q", issuer)
	}
	assert.False(t, NewKeycloakIdentityRepository(cfg, nil, nil).Available(context.Background()))
}

// Removing a user from a group goes through the membership, keyed by ids.
func TestKeycloakDeleteGroupBindingUsesIDs(t *testing.T) {
	removed := false
	cfg, _ := newFakeKeycloak(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/admin/realms/okdp/users", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]kcUser{{ID: "u1", Username: "jdoe"}})
		})
		mux.HandleFunc("/admin/realms/okdp/groups", func(w http.ResponseWriter, r *http.Request) {
			// Keycloak's search is fuzzy: the exact name must be picked.
			_ = json.NewEncoder(w).Encode([]kcGroup{{ID: "g0", Name: "team-a-ops"}, {ID: "g1", Name: "team-a"}})
		})
		mux.HandleFunc("/admin/realms/okdp/users/u1/groups/g1", func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodDelete, r.Method)
			removed = true
			w.WriteHeader(http.StatusNoContent)
		})
	})

	repo := NewKeycloakIdentityRepository(cfg, nil, nil)
	require.NoError(t, repo.DeleteGroupBindingByRef(context.Background(), "jdoe", "team-a"))
	assert.True(t, removed)
}

// An empty realm lists as [] in JSON, not null: the console iterates the answer.
func TestKeycloakEmptyListsAreEmptyNotNil(t *testing.T) {
	cfg, _ := newFakeKeycloak(t, func(mux *http.ServeMux) {
		for _, route := range []string{"/admin/realms/okdp/users", "/admin/realms/okdp/groups"} {
			mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("[]"))
			})
		}
	})
	repo := NewKeycloakIdentityRepository(cfg, nil, nil)

	users, err := repo.ListUsers(context.Background())
	require.NoError(t, err)
	groups, err := repo.ListGroups(context.Background())
	require.NoError(t, err)

	for name, list := range map[string]any{"users": users, "groups": groups} {
		data, err := json.Marshal(list)
		require.NoError(t, err)
		assert.Equal(t, "[]", string(data), name)
	}
}
