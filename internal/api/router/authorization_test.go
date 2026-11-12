package router

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	compiler "github.com/okdp/okdp-compiler"
	"github.com/okdp/okdp-gitops/gitops"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okdp/okdp-control-plane-server/internal/api/handlers"
	"github.com/okdp/okdp-control-plane-server/internal/api/middleware"
	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/authz"
	"github.com/okdp/okdp-control-plane-server/internal/config"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

// groupsVerifier accepts a token named after a caller of the matrix, its
// groups those of the caller.
type groupsVerifier map[string][]string

func (v groupsVerifier) Verify(_ context.Context, token string) (*auth.Identity, error) {
	groups, ok := v[token]
	if !ok {
		return nil, errors.New("unknown token")
	}
	return &auth.Identity{Subject: token, Username: token, Groups: groups}, nil
}

// The callers of the matrix: anonymous sends no token.
var callers = groupsVerifier{
	"plain":    {"somebody-else"},
	"viewer":   {"analysts"},
	"editor":   {"okdp-project-demo"},
	"admin":    {"demo-admins"},
	"platform": {"platform_admin"},
}

// authorizedRouter serves the API over a tree whose platform admins are
// platform_admin, and whose project demo gives analysts viewer,
// okdp-project-demo editor and demo-admins admin; project other has no
// access, its okdp-project-other an editor. Handlers but the projects'
// are nil: a request the authorization lets through panics into a 500,
// never a 401 or a 403.
func authorizedRouter(t *testing.T, verifier auth.Verifier) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	previous := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = io.Discard
	t.Cleanup(func() { gin.DefaultErrorWriter = previous })
	store := gitops.MustMemoryStore(map[string]string{
		compiler.PlatformFile:         "global:\n  okdp:\n    access:\n      admins: [platform_admin]\n",
		"projects/demo/project.yaml":  "access:\n  analysts: viewer\n  okdp-project-demo: editor\n  demo-admins: admin\n",
		"projects/other/project.yaml": "description: another team's\n",
	})
	deployments := gitops.NewDeployments(store)
	projects := handlers.NewProjectHandler(service.NewDefaultProjectService(nil, deployments))
	authority := authz.NewAuthority(authz.DeploymentsSource(deployments))
	return SetupRouter(&config.Config{}, verifier, authority, nil, projects, nil, nil, nil, nil, nil, nil, nil, nil)
}

// inProject turns a route pattern into a path of project demo, each
// parameter demo.
func inProject(pattern string) string {
	parts := strings.Split(pattern, "/")
	for i, p := range parts {
		if strings.HasPrefix(p, ":") {
			parts[i] = "demo"
		}
	}
	return strings.Join(parts, "/")
}

func serve(r *gin.Engine, method, target, token, body string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// Every route of the API has a permission, and every permission a route:
// a route added without one would answer 403 to everyone, an entry left
// behind would hide that a route is gone.
func TestEveryApiRouteHasAPermission(t *testing.T) {
	r := authorizedRouter(t, callers)
	registered := map[string]bool{}
	for _, rt := range r.Routes() {
		if !strings.HasPrefix(rt.Path, "/api") {
			continue
		}
		key := middleware.RouteKey(rt.Method, rt.Path)
		registered[key] = true
		if _, ok := Permissions[key]; !ok {
			t.Errorf("%s has no permission: it answers 403 to everyone", key)
		}
	}
	for key := range Permissions {
		if !registered[key] {
			t.Errorf("%s has a permission and no route", key)
		}
	}
	if len(registered) < 50 {
		t.Fatalf("only %d API routes, the router does not look fully registered", len(registered))
	}
}

// Deny by default: each route, its permission taken out of the table,
// answers 403 even to a platform admin, and even with authentication off.
func TestARouteMissingFromTheTableIsForbiddenToEveryone(t *testing.T) {
	for name, verifier := range map[string]auth.Verifier{"platform admin": callers, "authentication off": nil} {
		t.Run(name, func(t *testing.T) {
			r := authorizedRouter(t, verifier)
			checked := 0
			for _, rt := range r.Routes() {
				if !strings.HasPrefix(rt.Path, "/api") {
					continue
				}
				key := middleware.RouteKey(rt.Method, rt.Path)
				perm := Permissions[key]
				delete(Permissions, key)
				w := serve(r, rt.Method, inProject(rt.Path), "platform", "")
				Permissions[key] = perm
				checked++
				if w.Code != http.StatusForbidden {
					t.Errorf("%s out of the table answered %d, wanted 403", key, w.Code)
					continue
				}
				var body map[string]string
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
				assert.Equal(t, "forbidden", body["code"], key)
			}
			assert.Greater(t, checked, 50)
		})
	}
}

// The role matrix: who may call what.
func TestTheRoleMatrix(t *testing.T) {
	r := authorizedRouter(t, callers)
	everyone := []string{"anonymous", "plain", "viewer", "editor", "admin", "platform"}
	for _, tc := range []struct {
		method, target string
		allowed        []string
	}{
		{http.MethodGet, "/api/capabilities", everyone},
		{http.MethodGet, "/api/me", everyone[1:]},
		{http.MethodGet, "/api/projects", everyone[1:]},
		{http.MethodGet, "/api/platform-services", everyone[1:]},
		{http.MethodGet, "/api/contracts", everyone[1:]},
		{http.MethodGet, "/api/projects/demo", []string{"viewer", "editor", "admin", "platform"}},
		{http.MethodGet, "/api/projects/demo/services", []string{"viewer", "editor", "admin", "platform"}},
		{http.MethodGet, "/api/projects/demo/services/stream", []string{"viewer", "editor", "admin", "platform"}},
		{http.MethodPost, "/api/projects/demo/services/trino/sql", []string{"viewer", "editor", "admin", "platform"}},
		{http.MethodPost, "/api/projects/demo/services", []string{"editor", "admin", "platform"}},
		{http.MethodPatch, "/api/projects/demo/services/trino/parameters", []string{"editor", "admin", "platform"}},
		{http.MethodPost, "/api/projects/demo/connections/pg/sql", []string{"editor", "admin", "platform"}},
		{http.MethodDelete, "/api/projects/demo/claims/db", []string{"editor", "admin", "platform"}},
		{http.MethodPost, "/api/projects/demo/spark-apps", []string{"editor", "admin", "platform"}},
		{http.MethodPut, "/api/projects/demo", []string{"admin", "platform"}},
		{http.MethodGet, "/api/projects/other", []string{"platform"}},
		{http.MethodPost, "/api/projects/other/services", []string{"platform"}},
		{http.MethodPost, "/api/projects", []string{"platform"}},
		{http.MethodDelete, "/api/projects/demo", []string{"platform"}},
		{http.MethodPost, "/api/platform-services", []string{"platform"}},
		{http.MethodDelete, "/api/platform-services/trino", []string{"platform"}},
		{http.MethodGet, "/api/v1/identity/users", []string{"platform"}},
		{http.MethodPut, "/api/v1/identity/users/plain", []string{"platform"}},
		{http.MethodPost, "/api/v1/identity/groups", []string{"platform"}},
	} {
		for _, who := range everyone {
			token := who
			if who == "anonymous" {
				token = ""
			}
			w := serve(r, tc.method, tc.target, token, "")
			allowed := false
			for _, a := range tc.allowed {
				allowed = allowed || a == who
			}
			switch {
			case allowed && (w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden):
				t.Errorf("%s %s as %s answered %d, wanted it let through", tc.method, tc.target, who, w.Code)
			case !allowed && who == "anonymous" && w.Code != http.StatusUnauthorized:
				t.Errorf("%s %s as anonymous answered %d, wanted 401", tc.method, tc.target, w.Code)
			case !allowed && who != "anonymous" && w.Code != http.StatusForbidden:
				t.Errorf("%s %s as %s answered %d, wanted 403", tc.method, tc.target, who, w.Code)
			}
		}
	}
}

// The self-escalation of SRV-01: a user who is no platform admin cannot
// put itself in the platform admins' group, whatever its project roles.
func TestAUserCannotMakeItselfPlatformAdmin(t *testing.T) {
	r := authorizedRouter(t, callers)
	for _, who := range []string{"plain", "viewer", "editor", "admin"} {
		w := serve(r, http.MethodPut, "/api/v1/identity/users/"+who, who, `{"username":"`+who+`","groups":["platform_admin"]}`)
		require.Equal(t, http.StatusForbidden, w.Code, who)
		assert.JSONEq(t, `{"error":"this needs the role platform admin","code":"forbidden"}`, w.Body.String())
	}
}

// The lists of projects show the caller's only, every one to a platform
// admin; /api/me tells the console what the caller may do.
func TestTheCallerSeesItsProjectsAndItsRights(t *testing.T) {
	r := authorizedRouter(t, callers)
	for who, want := range map[string][]string{
		"plain":    {},
		"viewer":   {"demo"},
		"admin":    {"demo"},
		"platform": {"demo", "other"},
	} {
		w := serve(r, http.MethodGet, "/api/projects", who, "")
		require.Equal(t, http.StatusOK, w.Code, who)
		var projects []models.Project
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &projects))
		names := []string{}
		for _, p := range projects {
			names = append(names, p.Name)
		}
		sort.Strings(names)
		assert.Equal(t, want, names, who)
	}

	for who, want := range map[string]models.CallerAccess{
		"plain":    {Username: "plain", Groups: []string{"somebody-else"}, Projects: map[string]string{}},
		"editor":   {Username: "editor", Groups: []string{"okdp-project-demo"}, Projects: map[string]string{"demo": "editor"}},
		"platform": {Username: "platform", Groups: []string{"platform_admin"}, PlatformAdmin: true, Projects: map[string]string{"demo": "admin", "other": "admin"}},
	} {
		w := serve(r, http.MethodGet, "/api/me", who, "")
		require.Equal(t, http.StatusOK, w.Code, who)
		var got models.CallerAccess
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		assert.Equal(t, want, got, who)
	}
}
