package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okdp/okdp-control-plane-server/internal/apperr"
	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/netguard"
)

// fakeTrino serves three pages of two rows. Its nextUri points to an
// in-cluster host, as a Trino that ignores forwarded headers would.
type fakeTrino struct {
	mu      sync.Mutex
	auths   []string
	deletes int
	query   string
}

func (f *fakeTrino) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	page := map[string]any{"id": "q1"}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/statement":
		body, _ := io.ReadAll(r.Body)
		f.query = string(body)
		if f.query == "SELEC 1" {
			page["error"] = map[string]any{"message": "mismatched input 'SELEC'", "errorName": "SYNTAX_ERROR",
				"errorLocation": map[string]any{"lineNumber": 1, "columnNumber": 1}}
			break
		}
		page["nextUri"] = "http://trino.internal:8080/v1/statement/executing/q1/1?slug=x"
	case r.Method == http.MethodGet && r.URL.Path == "/v1/statement/executing/q1/1":
		page["columns"] = []map[string]string{{"name": "n", "type": "integer"}}
		page["data"] = [][]any{{1}, {2}}
		page["nextUri"] = "http://trino.internal:8080/v1/statement/executing/q1/2?slug=x"
	case r.Method == http.MethodGet && r.URL.Path == "/v1/statement/executing/q1/2":
		page["data"] = [][]any{{3}, {4}}
		page["nextUri"] = "http://trino.internal:8080/v1/statement/executing/q1/3?slug=x"
	case r.Method == http.MethodGet && r.URL.Path == "/v1/statement/executing/q1/3":
		page["data"] = [][]any{{5}, {6}}
	case r.Method == http.MethodDelete:
		f.deletes++
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(page)
}

func sqlServiceFor(url, service string) *DefaultSqlService {
	return NewDefaultSqlService(func(_ context.Context, project, name string) (*models.ServiceInstance, error) {
		if name == "missing" {
			return nil, apperr.NotFound(servicesResource, name)
		}
		return &models.ServiceInstance{Name: name, Service: service, URL: url}, nil
	}, SqlConnectionSource{}, nil, SqlOptions{Guard: loopbackAllowed()})
}

// loopbackAllowed is the guard of a test whose servers listen on the
// loopback: the platform's allow-list naming it.
func loopbackAllowed() *netguard.Guard {
	g, err := netguard.New([]string{"127.0.0.0/8", "::1"}, nil)
	if err != nil {
		panic(err)
	}
	return g
}

func TestSqlFollowsPagesOnTheInstanceURL(t *testing.T) {
	trino := &fakeTrino{}
	srv := httptest.NewServer(trino)
	defer srv.Close()

	res, err := sqlServiceWith(srv.URL+"/", "trino", &fakeExchange{token: "tok"}).ExecuteQuery(context.Background(), "p", "t", "Bearer caller", models.SqlQueryRequest{Query: "SELECT n"})
	if err != nil {
		t.Fatal(err)
	}
	if res.RowCount != 6 || res.Truncated || res.QueryID != "q1" || res.Error != nil {
		t.Fatalf("unexpected result %+v", res)
	}
	if len(res.Columns) != 1 || res.Columns[0] != (models.SqlColumn{Name: "n", Type: "integer"}) {
		t.Fatalf("columns %+v", res.Columns)
	}
	if trino.query != "SELECT n" {
		t.Fatalf("query %q", trino.query)
	}
	if len(trino.auths) != 4 {
		t.Fatalf("expected 4 calls on the instance URL, got %d", len(trino.auths))
	}
	for _, a := range trino.auths {
		if a != "Bearer tok" {
			t.Fatalf("the exchanged token not on every page: %q", a)
		}
	}
}

func TestSqlTruncatesAndCancels(t *testing.T) {
	trino := &fakeTrino{}
	srv := httptest.NewServer(trino)
	defer srv.Close()

	res, err := sqlServiceFor(srv.URL, "trino").ExecuteQuery(context.Background(), "p", "t", "", models.SqlQueryRequest{Query: "SELECT n", MaxRows: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.RowCount != 3 || !res.Truncated || res.TruncatedBy != models.SqlTruncatedByRows || res.MaxRows != 3 {
		t.Fatalf("unexpected result %+v", res)
	}
	if trino.deletes != 1 {
		t.Fatalf("expected the query to be cancelled once, got %d", trino.deletes)
	}

	// The size cap, the row cap told apart; the cap applied, the default
	// one or the largest.
	svc := sqlServiceFor(srv.URL, "trino")
	svc.limits.MaxResultBytes = 1
	res, err = svc.ExecuteQuery(context.Background(), "p", "t", "", models.SqlQueryRequest{Query: "SELECT n", MaxRows: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.TruncatedBy != models.SqlTruncatedByBytes || res.MaxRows != 10000 {
		t.Fatalf("unexpected result %+v", res)
	}
	res, err = sqlServiceFor(srv.URL, "trino").ExecuteQuery(context.Background(), "p", "t", "", models.SqlQueryRequest{Query: "SELECT n"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Truncated || res.TruncatedBy != "" || res.MaxRows != 1000 {
		t.Fatalf("unexpected result %+v", res)
	}
}

// A Trino statement past the statement timeout is a coded error, not a
// failure of the engine.
func TestSqlTimeoutIsCoded(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	svc := NewDefaultSqlService(func(context.Context, string, string) (*models.ServiceInstance, error) {
		return &models.ServiceInstance{Name: "t", Service: "trino", URL: srv.URL}, nil
	}, SqlConnectionSource{}, nil, SqlOptions{Guard: loopbackAllowed(), Limits: SqlLimits{StatementTimeout: 50 * time.Millisecond}})
	_, err := svc.ExecuteQuery(context.Background(), "p", "t", "", models.SqlQueryRequest{Query: "SELECT 1"})
	if !errors.Is(err, ErrSqlTimeout) {
		t.Fatalf("got %v, want ErrSqlTimeout", err)
	}

	// A client that goes away is no timeout.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = svc.ExecuteQuery(ctx, "p", "t", "", models.SqlQueryRequest{Query: "SELECT 1"})
	if err == nil || errors.Is(err, ErrSqlTimeout) {
		t.Fatalf("got %v, want the client's cancellation", err)
	}
}

func TestSqlEngineErrorIsAResult(t *testing.T) {
	srv := httptest.NewServer(&fakeTrino{})
	defer srv.Close()

	res, err := sqlServiceFor(srv.URL, "trino").ExecuteQuery(context.Background(), "p", "t", "", models.SqlQueryRequest{Query: "SELEC 1"})
	if err != nil {
		t.Fatal(err)
	}
	want := models.SqlQueryError{Message: "mismatched input 'SELEC'", ErrorName: "SYNTAX_ERROR", LineNumber: 1, ColumnNumber: 1}
	if res.Error == nil || *res.Error != want || res.Rows == nil || res.Columns == nil {
		t.Fatalf("unexpected result %+v", res)
	}
}

func TestSqlRejectedCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := sqlServiceWith(srv.URL, "trino", &fakeExchange{token: "x"}).ExecuteQuery(context.Background(), "p", "t", "Bearer x", models.SqlQueryRequest{Query: "SELECT 1"})
	if err == nil || errors.Is(err, ErrSqlUnsupported) {
		t.Fatalf("expected an engine error, got %v", err)
	}
}

func TestSqlResolutionErrors(t *testing.T) {
	ctx := context.Background()
	req := models.SqlQueryRequest{Query: "SELECT 1"}

	// DuckDB 2.0 speaks Quack (DuckDB's own serialization): no driver.
	if _, err := sqlServiceFor("https://duck.example", "duckdb").ExecuteQuery(ctx, "p", "d", "", req); !errors.Is(err, ErrSqlUnsupported) {
		t.Fatalf("duckdb: expected ErrSqlUnsupported, got %v", err)
	}
	if _, err := sqlServiceFor("", "trino").ExecuteQuery(ctx, "p", "t", "", req); !errors.Is(err, ErrSqlNoURL) {
		t.Fatalf("expected ErrSqlNoURL, got %v", err)
	}
	if _, err := sqlServiceFor("https://x", "trino").ExecuteQuery(ctx, "p", "missing", "", req); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

// fakeExchange records the subject of an exchange and gives a token of its
// own.
type fakeExchange struct {
	subject string
	token   string
	err     error
}

func (f *fakeExchange) Exchange(_ context.Context, subject string) (string, error) {
	f.subject = subject
	return f.token, f.err
}

// The caller's own token never reaches Trino: the engine gets what the
// exchange gives (SRV-22); without an exchange the statement is refused,
// without a token (AUTH_DISABLED) nothing is sent.
func TestSqlForwardsOnlyTheExchangedToken(t *testing.T) {
	trino := &fakeTrino{}
	srv := httptest.NewServer(trino)
	defer srv.Close()
	seen := func() string {
		trino.mu.Lock()
		defer trino.mu.Unlock()
		if len(trino.auths) == 0 {
			return ""
		}
		return trino.auths[len(trino.auths)-1]
	}
	reset := func() { trino.mu.Lock(); trino.auths = nil; trino.mu.Unlock() }
	statement := models.SqlQueryRequest{Query: "SELECT 1"}

	exchange := &fakeExchange{token: "engine-token"}
	svc := sqlServiceWith(srv.URL, "trino", exchange)
	_, err := svc.ExecuteQuery(context.Background(), "p", "t", "Bearer caller-token", statement)
	require.NoError(t, err)
	assert.Equal(t, "caller-token", exchange.subject)
	assert.Equal(t, "Bearer engine-token", seen())

	// The exchange is refused: a coded error, nothing forwarded.
	reset()
	exchange = &fakeExchange{err: errors.New("invalid_token")}
	_, err = sqlServiceWith(srv.URL, "trino", exchange).ExecuteQuery(context.Background(), "p", "t", "Bearer caller-token", statement)
	assert.ErrorIs(t, err, ErrSqlTokenExchange)
	assert.Empty(t, seen())

	// None configured: never the caller's own token.
	reset()
	_, err = sqlServiceWith(srv.URL, "trino", nil).ExecuteQuery(context.Background(), "p", "t", "Bearer caller-token", statement)
	assert.ErrorIs(t, err, ErrSqlNoTokenExchange)
	assert.Empty(t, seen())

	// No token at all (AUTH_DISABLED): nothing sent, no exchange asked.
	exchange = &fakeExchange{token: "engine-token"}
	_, err = sqlServiceWith(srv.URL, "trino", exchange).ExecuteQuery(context.Background(), "p", "t", "", statement)
	require.NoError(t, err)
	assert.Empty(t, seen())
	assert.Empty(t, exchange.subject)
}

// sqlServiceWith is sqlServiceFor with the token exchange of the
// statements on Trino.
func sqlServiceWith(url, service string, exchange auth.TokenExchanger) *DefaultSqlService {
	svc := sqlServiceFor(url, service)
	svc.exchange = exchange
	return svc
}
