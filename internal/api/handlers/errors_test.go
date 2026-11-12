package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/okdp/okdp-control-plane-server/internal/api/middleware"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

func answer(t *testing.T, err error) (int, map[string]any, string) {
	t.Helper()
	engine := gin.New()
	engine.Use(middleware.RequestID())
	engine.GET("/x", func(c *gin.Context) { writeError(c, err) })
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/x", nil))
	body := map[string]any{}
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	return recorder.Code, body, recorder.Header().Get(middleware.RequestIDHeader)
}

// An internal error answers a generic message and the request id: what
// Kubernetes, Git or Keycloak said stays in the log (SRV-10).
func TestAnInternalErrorKeepsItsCauseInTheLog(t *testing.T) {
	status, body, id := answer(t, errors.New(`secrets "openbao-init" is forbidden: User "system:serviceaccount:okdp:server" cannot get`))
	if status != http.StatusInternalServerError || body["requestId"] != id || id == "" {
		t.Fatalf("got %d %v (id %q)", status, body, id)
	}
	if msg, _ := body["error"].(string); strings.Contains(msg, "openbao") || strings.Contains(msg, "forbidden") {
		t.Fatalf("the cause reached the client: %q", msg)
	}
}

func TestDomainErrorsKeepTheirStatus(t *testing.T) {
	gr := schema.GroupResource{Group: "okdp.io", Resource: "connections"}
	for _, tc := range []struct {
		err    error
		status int
	}{
		{apierrors.NewNotFound(gr, "w"), http.StatusNotFound},
		{apierrors.NewAlreadyExists(gr, "w"), http.StatusConflict},
		{fmt.Errorf("x: %w", service.ErrConnectionsUnavailable), http.StatusNotImplemented},
		{apierrors.NewBadRequest("spec.image: required"), http.StatusBadRequest},
	} {
		if status, body, _ := answer(t, tc.err); status != tc.status || body["error"] != tc.err.Error() {
			t.Errorf("%v: got %d %v, want %d", tc.err, status, body, tc.status)
		}
	}
}

func TestARequestIdIsAPlainToken(t *testing.T) {
	engine := gin.New()
	engine.Use(middleware.RequestID())
	engine.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, middleware.RequestIDOf(c)) })
	for sent, kept := range map[string]bool{"abcdef0123456789": true, "a b\nlog-injection": false, "short": false} {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set(middleware.RequestIDHeader, sent)
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)
		if (recorder.Body.String() == sent) != kept || recorder.Header().Get(middleware.RequestIDHeader) != recorder.Body.String() {
			t.Errorf("%q: answered %q", sent, recorder.Body.String())
		}
	}
}
