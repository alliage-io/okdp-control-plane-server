package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/netguard"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

type sqlSpy struct {
	service.SqlService
	err error
	req models.SqlQueryRequest
}

func (s *sqlSpy) ExecuteOnConnection(_ context.Context, _, _ string, req models.SqlQueryRequest) (*models.SqlQueryResult, error) {
	s.req = req
	if s.err != nil {
		return nil, s.err
	}
	return &models.SqlQueryResult{Columns: []models.SqlColumn{}, Rows: [][]any{}}, nil
}

func runSql(t *testing.T, spy *sqlSpy, body string) (int, map[string]any) {
	t.Helper()
	engine := gin.New()
	engine.POST("/api/projects/:name/connections/:connName/sql", NewSqlHandler(spy).ExecuteOnConnection)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/projects/a/connections/w/sql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(recorder, req)
	out := map[string]any{}
	_ = json.Unmarshal(recorder.Body.Bytes(), &out)
	return recorder.Code, out
}

// The refusals of the editor carry a code the console tells apart.
func TestSqlRefusalsCarryTheirCode(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("secret 'x' %w", service.ErrSqlCredentialsRefused), http.StatusBadRequest, ErrorCodeSqlCredentialsRefused},
		{fmt.Errorf("claim 'x' %w", service.ErrSqlUnbound), http.StatusBadRequest, ErrorCodeSqlClaimUnbound},
		{fmt.Errorf("dial: %w", netguard.ErrRefused), http.StatusBadRequest, ErrorCodeSqlAddressRefused},
		{service.ErrSqlBusy, http.StatusTooManyRequests, ErrorCodeSqlBusy},
		{service.ErrSqlWriteRefused, http.StatusForbidden, ErrorCodeSqlWriteRefused},
		{fmt.Errorf("engine unreachable: %w", service.ErrSqlTLS), http.StatusBadGateway, ErrorCodeSqlTLS},
		{fmt.Errorf("engine unreachable: %w", service.ErrSqlAuthMethod), http.StatusBadGateway, ErrorCodeSqlAuthMethod},
	} {
		status, body := runSql(t, &sqlSpy{err: tc.err}, `{"query":"SELECT 1"}`)
		if status != tc.status || body["code"] != tc.code {
			t.Errorf("%v: got %d %v, want %d %s", tc.err, status, body, tc.status, tc.code)
		}
	}
}

func TestSqlWriteFlagReachesTheService(t *testing.T) {
	spy := &sqlSpy{}
	if status, _ := runSql(t, spy, `{"query":"DELETE FROM t","write":true}`); status != http.StatusOK || !spy.req.Write {
		t.Fatalf("status %d, write %v", status, spy.req.Write)
	}
	if _, _ = runSql(t, spy, `{"query":"SELECT 1"}`); spy.req.Write {
		t.Fatal("read-only unless asked")
	}
}
