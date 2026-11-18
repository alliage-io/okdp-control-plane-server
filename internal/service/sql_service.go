package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	compiler "github.com/okdp/okdp-compiler"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/netguard"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

// sqlMaxPageBytes caps one engine response page.
const sqlMaxPageBytes = 32 << 20

var (
	// ErrSqlUnsupported: the instance's service has no SQL engine driver.
	ErrSqlUnsupported = errors.New("does not support SQL execution")
	// ErrSqlNoURL: the instance publishes no URL (its okdp.info.yaml).
	ErrSqlNoURL = errors.New("does not expose a URL")
	// ErrSqlNoCredentials: the connection's credentials Secret is missing.
	ErrSqlNoCredentials = errors.New("does not exist")
	// ErrSqlBusy: the caller already runs as many statements as allowed.
	ErrSqlBusy = errors.New("too many statements running for this user, retry when one has finished")
	// ErrSqlWriteRefused: the caller may not run a statement in write mode.
	ErrSqlWriteRefused = errors.New("write mode is not allowed")
	// ErrSqlTimeout: the engine did not answer within the statement
	// timeout (SQL_STATEMENT_TIMEOUT); the statement was cancelled.
	ErrSqlTimeout = errors.New("the statement did not finish within the statement timeout and was cancelled")
	// ErrSqlNoTokenExchange: the caller's token would have to be forwarded
	// (it is accepted at /api, a replay for whoever controls the engine)
	// and no token exchange is configured.
	ErrSqlNoTokenExchange = errors.New("the token exchange of the SQL editor is not configured (SQL_TOKEN_EXCHANGE_AUDIENCE and the Keycloak client's secret): the caller's token is not forwarded to the engine")
	// ErrSqlTokenExchange: the identity provider refused or failed the
	// exchange.
	ErrSqlTokenExchange = errors.New("the identity provider gave no token for the engine")
)

// SqlLimits bounds the SQL editor; a zero field takes its default.
type SqlLimits struct {
	// StatementTimeout bounds one execution (2 minutes).
	StatementTimeout time.Duration
	// DefaultRows is the row cap of a request that names none (1000);
	// MaxRows the largest one a request may ask (10000).
	DefaultRows, MaxRows int
	// MaxResultBytes caps the size of the rows of a result (16 MiB).
	MaxResultBytes int
	// MaxConcurrentPerUser is how many statements one user runs at once (2).
	MaxConcurrentPerUser int
}

func (l SqlLimits) withDefaults() SqlLimits {
	if l.StatementTimeout <= 0 {
		l.StatementTimeout = 2 * time.Minute
	}
	if l.MaxRows <= 0 {
		l.MaxRows = 10000
	}
	if l.DefaultRows <= 0 {
		l.DefaultRows = 1000
	}
	if l.DefaultRows > l.MaxRows {
		l.DefaultRows = l.MaxRows
	}
	if l.MaxResultBytes <= 0 {
		l.MaxResultBytes = 16 << 20
	}
	if l.MaxConcurrentPerUser <= 0 {
		l.MaxConcurrentPerUser = 2
	}
	return l
}

// SqlOptions are the bounds and the network policy of the SQL editor.
type SqlOptions struct {
	Limits SqlLimits
	// Guard refuses the addresses a statement may not reach (loopback,
	// link-local, the API server); nil is netguard.Default.
	Guard *netguard.Guard
	// RootCAs verify the database and engine certificates: the system's
	// and the platform's CA bundle; nil is the system's.
	RootCAs *x509.CertPool
	// AuthorizeWrite decides whether the caller may run a statement in
	// write mode on a project's connection; nil allows every
	// authenticated caller.
	AuthorizeWrite func(ctx context.Context, project string) error
	// TokenExchange trades the caller's token for one of the engine's
	// audience, the only token forwarded to the engine (auth.TokenExchanger,
	// RFC 8693); nil, a statement on an engine that needs one is refused,
	// the caller's token never forwarded.
	TokenExchange auth.TokenExchanger
}

// SqlService executes SQL statements on deployed SQL engines.
type SqlService interface {
	// ExecuteQuery runs a statement on the given service instance. The
	// caller's Authorization header is exchanged for a token of the
	// engine's audience (auth.TokenExchanger), never forwarded: the
	// engine enforces
	// platform SSO itself.
	ExecuteQuery(ctx context.Context, project, serviceName, authorization string, req models.SqlQueryRequest) (*models.SqlQueryResult, error)
	// ExecuteOnConnection runs a statement on a connection of the project
	// (database-server, engine postgresql) declared in the tree, the
	// project's, the platform's or a named claim's, logged in with the
	// credentials of an allowed source (resolveTarget): read-only unless
	// the request asks for write mode.
	ExecuteOnConnection(ctx context.Context, project, connection string, req models.SqlQueryRequest) (*models.SqlQueryResult, error)
}

// SqlConnectionSource reads what the SQL service needs of the connections
// of the tree: their declarations in Git, the named claims of a project
// and the compile that binds them, the credentials Secrets, and the
// ExternalSecrets that fill the Secrets of the platform's secret store.
type SqlConnectionSource struct {
	// Get returns a connection of a project, the platform's for "".
	Get func(ctx context.Context, project, name string) (*compiler.Connection, error)
	// Claims returns the named claims of a project; Tree the compile of the
	// tree. Without them a named claim is not found.
	Claims func(ctx context.Context, project string) ([]compiler.ClaimSpec, error)
	Tree   func(ctx context.Context) (*compiler.Tree, error)
	// Secrets reads the credentials Secrets.
	Secrets repository.ConnectionSecretRepository
	// ExternalSecrets lists the ExternalSecrets of a namespace; without it
	// the credentials of a secretRef.remoteKey are not found.
	ExternalSecrets func(ctx context.Context, namespace string) ([]crd.ESOExternalSecret, error)
}

// sqlEngine is the driver of one HTTP wire protocol of a deployed instance.
// Engines are keyed by the catalog service name in DefaultSqlService.engines;
// a service without an entry answers ErrSqlUnsupported. PostgreSQL is not an
// instance but an external connection: see ExecuteOnConnection.
//
// Only Trino has a driver. DuckDB (community package duckdb, DuckDB 2.0
// served over Quack) cannot get one here: Quack encodes messages with DuckDB's
// internal serialization (application/duckdb), so its only client is DuckDB
// itself, which this CGO-free server does not embed. Its authentication is
// also a shared token Secret, not the caller's bearer token.
type sqlEngine interface {
	execute(ctx context.Context, client *http.Client, baseURL, authorization, query string, caps sqlCaps) (*models.SqlQueryResult, error)
}

// sqlCaps are the caps of one execution.
type sqlCaps struct {
	rows, bytes int
}

type DefaultSqlService struct {
	getService func(ctx context.Context, project, name string) (*models.ServiceInstance, error)
	// insecureTLS reports whether engine certificates are taken on trust.
	insecureTLS func(ctx context.Context) bool
	engines     map[string]sqlEngine
	connections SqlConnectionSource
	client      *http.Client
	insecure    *http.Client
	limits      SqlLimits
	guard       *netguard.Guard
	rootCAs     *x509.CertPool
	authorize   func(ctx context.Context, project string) error
	running     *userSlots
	exchange    auth.TokenExchanger
}

// NewDefaultSqlService resolves instances through getService (the URL the
// instance publishes is the engine endpoint). insecureTLS may be nil
// (certificates checked).
func NewDefaultSqlService(getService func(ctx context.Context, project, name string) (*models.ServiceInstance, error), connections SqlConnectionSource, insecureTLS func(ctx context.Context) bool, opts SqlOptions) *DefaultSqlService {
	guard := opts.Guard
	if guard == nil {
		guard = netguard.Default()
	}
	limits := opts.Limits.withDefaults()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = guard.DialContext
	transport.TLSClientConfig = &tls.Config{RootCAs: opts.RootCAs, MinVersion: tls.VersionTLS12}
	insecureTransport := transport.Clone()
	insecureTransport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} // #nosec G402 -- opt-in, sandboxes
	return &DefaultSqlService{
		getService:  getService,
		insecureTLS: insecureTLS,
		engines:     map[string]sqlEngine{"trino": trinoEngine{}},
		connections: connections,
		client:      &http.Client{Timeout: 30 * time.Second, Transport: transport},
		insecure:    &http.Client{Timeout: 30 * time.Second, Transport: insecureTransport},
		limits:      limits,
		guard:       guard,
		rootCAs:     opts.RootCAs,
		authorize:   opts.AuthorizeWrite,
		running:     &userSlots{max: limits.MaxConcurrentPerUser, n: map[string]int{}},
		exchange:    opts.TokenExchange,
	}
}

func (s *DefaultSqlService) ExecuteQuery(ctx context.Context, project, serviceName, authorization string, req models.SqlQueryRequest) (*models.SqlQueryResult, error) {
	instance, err := s.getService(ctx, project, serviceName)
	if err != nil {
		return nil, err
	}
	engine, ok := s.engines[instance.Service]
	if !ok {
		return nil, fmt.Errorf("service '%s' (%s) %w", serviceName, instance.Service, ErrSqlUnsupported)
	}
	if instance.URL == "" {
		return nil, fmt.Errorf("service '%s' %w", serviceName, ErrSqlNoURL)
	}

	client := s.client
	if s.insecureTLS != nil && s.insecureTLS(ctx) {
		client = s.insecure
	}

	// The engine authorizes a token of its own audience for the caller,
	// never the caller's own, which the API would accept back; the mode
	// is the engine's, the write flag is not read.
	audit := sqlAudit(ctx, project, "service", serviceName, instance.Service, false, req.Query)
	return s.timed(ctx, audit, req, func(ctx context.Context, caps sqlCaps) (*models.SqlQueryResult, error) {
		engineToken, err := s.engineToken(ctx, authorization)
		if err != nil {
			return nil, err
		}
		return engine.execute(ctx, client, instance.URL, engineToken, req.Query, caps)
	})
}

// engineToken is the Authorization of a statement on an engine: the
// caller's bearer token exchanged (SqlOptions.TokenExchange), nothing
// when the request has none (AUTH_DISABLED).
func (s *DefaultSqlService) engineToken(ctx context.Context, authorization string) (string, error) {
	if authorization == "" {
		return "", nil
	}
	if s.exchange == nil {
		return "", ErrSqlNoTokenExchange
	}
	subject := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	token, err := s.exchange.Exchange(ctx, subject)
	if err != nil {
		slog.Warn("Could not exchange the caller's token for the SQL engine", "error", err)
		return "", ErrSqlTokenExchange
	}
	return "Bearer " + token, nil
}

func (s *DefaultSqlService) ExecuteOnConnection(ctx context.Context, project, name string, req models.SqlQueryRequest) (*models.SqlQueryResult, error) {
	if s.connections.Get == nil {
		return nil, ErrConnectionsUnavailable
	}
	target, err := s.resolveTarget(ctx, project, name)
	if err != nil {
		return nil, err
	}
	if req.Write {
		if err := s.authorizeWrite(ctx, project); err != nil {
			return nil, err
		}
	}
	values := connectionValues{}
	for k, v := range target.values {
		values[k] = v
	}
	for k, v := range target.credentials {
		values[k] = string(v)
	}
	audit := sqlAudit(ctx, project, "connection", name, target.scope, req.Write, req.Query)
	return s.timed(ctx, audit, req, func(ctx context.Context, caps sqlCaps) (*models.SqlQueryResult, error) {
		return s.runPostgres(ctx, values, req.Query, req.Write, caps)
	})
}

// authorizeWrite is the gate of write mode: SqlOptions.AuthorizeWrite,
// the project editor role (the server's main wires authz.Require).
func (s *DefaultSqlService) authorizeWrite(ctx context.Context, project string) error {
	if s.authorize == nil {
		return nil
	}
	if err := s.authorize(ctx, project); err != nil {
		// Both kept: the refusal's code, and the permission refused
		// (an *authz.ForbiddenError) for its 403.
		return fmt.Errorf("%w: %w", ErrSqlWriteRefused, err)
	}
	return nil
}

// userSlots counts the statements each user runs.
type userSlots struct {
	mu  sync.Mutex
	max int
	n   map[string]int
}

// acquire takes a slot of user, false when the user has none left.
func (u *userSlots) acquire(user string) (func(), bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.n[user] >= u.max {
		return nil, false
	}
	u.n[user]++
	return func() {
		u.mu.Lock()
		defer u.mu.Unlock()
		if u.n[user]--; u.n[user] <= 0 {
			delete(u.n, user)
		}
	}, true
}

// sqlAudit is the log entry of a statement: who runs it, where, in which
// mode, and the start of its text (sqlStatementForLog).
func sqlAudit(ctx context.Context, project, kind, name, scope string, write bool, query string) *slog.Logger {
	mode := "read-only"
	if write {
		mode = "write"
	}
	return slog.With(
		"user", auth.ActorName(ctx),
		"project", project,
		kind, name,
		"scope", scope,
		"mode", mode,
		"statement", sqlStatementForLog(query),
		"length", len(query),
	)
}

// sqlStatementLogRunes is how much of a statement the log keeps.
const sqlStatementLogRunes = 64

// sqlStatementForLog is the start of a statement, on one line: its first
// sqlStatementLogRunes characters, only its first word when it may carry
// a secret (a password, a key).
func sqlStatementForLog(query string) string {
	text := strings.Join(strings.Fields(query), " ")
	lower := strings.ToLower(text)
	for _, word := range []string{"password", "secret", "identified", "credential", "key", "token"} {
		if strings.Contains(lower, word) {
			first, _, _ := strings.Cut(text, " ")
			return first + " …"
		}
	}
	if r := []rune(text); len(r) > sqlStatementLogRunes {
		return string(r[:sqlStatementLogRunes]) + "…"
	}
	return text
}

// timed takes a slot of the caller (ErrSqlBusy past the limit), clamps
// the row cap, bounds the execution by the statement timeout (and by the
// request's context: a client that goes away cancels the statement), logs
// it and fills the counters every engine shares.
func (s *DefaultSqlService) timed(ctx context.Context, audit *slog.Logger, req models.SqlQueryRequest, run func(ctx context.Context, caps sqlCaps) (*models.SqlQueryResult, error)) (*models.SqlQueryResult, error) {
	release, ok := s.running.acquire(auth.ActorName(ctx))
	if !ok {
		audit.Warn("SQL statement refused: too many running for this user")
		return nil, ErrSqlBusy
	}
	defer release()
	caps := sqlCaps{rows: req.MaxRows, bytes: s.limits.MaxResultBytes}
	if caps.rows <= 0 {
		caps.rows = s.limits.DefaultRows
	}
	if caps.rows > s.limits.MaxRows {
		caps.rows = s.limits.MaxRows
	}
	timed, cancel := context.WithTimeout(ctx, s.limits.StatementTimeout)
	defer cancel()
	start := time.Now()
	audit.Info("SQL statement")
	result, err := run(timed, caps)
	elapsed := time.Since(start)
	if err != nil {
		audit.Warn("SQL statement failed", "error", err, "elapsedMs", elapsed.Milliseconds())
		if ctx.Err() == nil && errors.Is(timed.Err(), context.DeadlineExceeded) {
			// The statement's timeout, not the client gone.
			return nil, fmt.Errorf("%w (%s)", ErrSqlTimeout, s.limits.StatementTimeout)
		}
		return nil, err
	}
	result.RowCount = len(result.Rows)
	result.ElapsedMs = elapsed.Milliseconds()
	result.MaxRows = caps.rows
	result.Truncated = result.TruncatedBy != ""
	done := audit.With("elapsedMs", result.ElapsedMs, "rows", result.RowCount, "truncated", result.Truncated)
	if result.Error != nil {
		done = done.With("errorName", result.Error.ErrorName)
	}
	done.Info("SQL statement done")
	return result, nil
}

// trinoEngine speaks the Trino client protocol: POST /v1/statement, then GET
// each nextUri until there is none.
type trinoEngine struct{}

// trinoResponse is the subset of the Trino client protocol the proxy reads.
type trinoResponse struct {
	ID      string `json:"id"`
	NextURI string `json:"nextUri"`
	Columns []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"columns"`
	Data  [][]any `json:"data"`
	Error *struct {
		Message       string `json:"message"`
		ErrorName     string `json:"errorName"`
		ErrorLocation *struct {
			LineNumber   int `json:"lineNumber"`
			ColumnNumber int `json:"columnNumber"`
		} `json:"errorLocation"`
	} `json:"error"`
}

func (e trinoEngine) execute(ctx context.Context, client *http.Client, baseURL, authorization, query string, caps sqlCaps) (*models.SqlQueryResult, error) {
	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || base.Host == "" {
		return nil, fmt.Errorf("invalid engine URL %q", baseURL)
	}
	result := &models.SqlQueryResult{Columns: []models.SqlColumn{}, Rows: [][]any{}}

	page, size, err := e.call(ctx, client, http.MethodPost, base.String()+"/v1/statement", authorization, query)
	if err != nil {
		return nil, err
	}
	total := 0
	for {
		if page.ID != "" {
			result.QueryID = page.ID
		}
		if len(page.Columns) > 0 && len(result.Columns) == 0 {
			for _, col := range page.Columns {
				result.Columns = append(result.Columns, models.SqlColumn{Name: col.Name, Type: col.Type})
			}
		}
		// The size of a page's rows is the size of the page, near enough.
		total += size
		for _, row := range page.Data {
			if len(result.Rows) >= caps.rows {
				result.TruncatedBy = models.SqlTruncatedByRows
				break
			}
			if total > caps.bytes {
				result.TruncatedBy = models.SqlTruncatedByBytes
				break
			}
			result.Rows = append(result.Rows, row)
		}
		if page.Error != nil {
			result.Error = &models.SqlQueryError{
				Message:   page.Error.Message,
				ErrorName: page.Error.ErrorName,
			}
			if page.Error.ErrorLocation != nil {
				result.Error.LineNumber = page.Error.ErrorLocation.LineNumber
				result.Error.ColumnNumber = page.Error.ErrorLocation.ColumnNumber
			}
			break
		}
		if page.NextURI == "" {
			break
		}
		next, err := rebase(base, page.NextURI)
		if err != nil {
			return nil, err
		}
		if result.TruncatedBy != "" {
			// Enough rows: tell Trino to stop computing the rest.
			e.cancel(client, next, authorization)
			break
		}
		if page, size, err = e.call(ctx, client, http.MethodGet, next, authorization, ""); err != nil {
			if ctx.Err() != nil {
				// The client went away, or the statement timed out: stop
				// the query rather than let it run to its end.
				e.cancel(client, next, authorization)
			}
			return nil, err
		}
	}
	return result, nil
}

// rebase keeps the path and query of a nextUri but sends it to the engine
// URL the instance publishes. Trino builds nextUri from its own view of the
// request (an in-cluster http host unless it honors forwarded headers), and
// the caller's token must never follow a URI to another host.
func rebase(base *url.URL, nextURI string) (string, error) {
	next, err := url.Parse(nextURI)
	if err != nil {
		return "", fmt.Errorf("unexpected engine nextUri %q: %w", nextURI, err)
	}
	u := *base
	u.Path = strings.TrimRight(base.Path, "/") + next.Path
	u.RawPath = ""
	u.RawQuery = next.RawQuery
	u.Fragment = ""
	return u.String(), nil
}

// call returns a page and its size in bytes.
func (trinoEngine) call(ctx context.Context, client *http.Client, method, target, authorization, body string) (*trinoResponse, int, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, 0, err
	}
	if authorization != "" {
		httpReq.Header.Set("Authorization", authorization)
	}
	if body != "" {
		httpReq.Header.Set("Content-Type", "text/plain")
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("engine unreachable: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, sqlMaxPageBytes))
	if err != nil {
		return nil, 0, fmt.Errorf("reading engine response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, 0, fmt.Errorf("engine rejected the platform credentials (%d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("engine returned %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}

	var page trinoResponse
	if err := json.Unmarshal(payload, &page); err != nil {
		return nil, 0, fmt.Errorf("unexpected engine response: %w", err)
	}
	return &page, len(payload), nil
}

// cancel is best-effort: a DELETE on the nextUri stops the query.
func (trinoEngine) cancel(client *http.Client, nextURI, authorization string) {
	ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, nextURI, nil)
	if err != nil {
		return
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
	} else {
		slog.Debug("Failed to cancel SQL query", "error", err)
	}
}
