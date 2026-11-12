package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/netguard"
)

// runPostgres executes a statement on a PostgreSQL connection with the
// credentials of the connection, not the caller's: every project member
// using the editor acts as that database user.
//
// The connection is the editor's (editorConfig): TLS verified, SCRAM only,
// through the network guard, its statement_timeout the editor's.
//
// Read-only (the default), the statement runs alone through the extended
// protocol, which refuses a batch of several, in a READ ONLY transaction
// rolled back after it: a statement cannot leave the transaction's mode
// (default_transaction_read_only is set too, for what runs outside one).
// Read-only is a guard against mistakes, not the boundary: the boundary is
// what the database grants the connection's user.
//
// In write mode the batch goes through the simple query protocol, so
// several statements separated by ';' run in one call, in one implicit
// transaction unless the batch opens its own. The result is the last
// statement that returned columns (else the last statement), with its
// command tag.
//
// Rows past the caps are read and dropped, never cancelled: a cancel aborts
// the implicit transaction, which would roll back the writes of an earlier
// statement of the batch without saying so. The statement timeout bounds
// the cost.
func (s *DefaultSqlService) runPostgres(ctx context.Context, values connectionValues, query string, write bool, caps sqlCaps) (*models.SqlQueryResult, error) {
	config, err := s.editorConfig(values, write)
	if err != nil {
		return nil, err
	}

	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("engine unreachable: %w", editorConnectError(err))
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	result := &models.SqlQueryResult{Columns: []models.SqlColumn{}, Rows: [][]any{}}
	if !write {
		if _, err := conn.PgConn().Exec(ctx, "BEGIN TRANSACTION READ ONLY").ReadAll(); err != nil {
			return s.postgresFailure(ctx, query, err)
		}
		rr := conn.PgConn().ExecParams(ctx, query, nil, nil, nil, nil)
		columns, rows, truncated := readPostgresRows(conn.TypeMap(), rr, caps)
		tag, err := rr.Close()
		if err != nil {
			return s.postgresFailure(ctx, query, err)
		}
		_, _ = conn.PgConn().Exec(ctx, "ROLLBACK").ReadAll()
		result.Columns, result.Rows, result.Truncated = columns, rows, truncated
		result.CommandTag = tag.String()
		return result, nil
	}

	haveColumns := false
	mrr := conn.PgConn().Exec(ctx, query)
	for mrr.NextResult() {
		rr := mrr.ResultReader()
		columns, rows, truncated := readPostgresRows(conn.TypeMap(), rr, caps)
		tag, err := rr.Close()
		if err != nil {
			_ = mrr.Close()
			return s.postgresFailure(ctx, query, err)
		}
		result.CommandTag = tag.String()
		if len(columns) > 0 || !haveColumns {
			haveColumns = haveColumns || len(columns) > 0
			result.Columns, result.Rows, result.Truncated = columns, rows, truncated
		}
	}
	if err := mrr.Close(); err != nil {
		return s.postgresFailure(ctx, query, err)
	}
	return result, nil
}

// readPostgresRows reads the rows of a result, the ones past the caps (rows,
// bytes of their values) dropped.
func readPostgresRows(typeMap *pgtype.Map, rr *pgconn.ResultReader, caps sqlCaps) ([]models.SqlColumn, [][]any, bool) {
	fields := rr.FieldDescriptions()
	columns := make([]models.SqlColumn, len(fields))
	for i, f := range fields {
		columns[i] = models.SqlColumn{Name: f.Name, Type: postgresTypeName(typeMap, f.DataTypeOID)}
	}
	rows := [][]any{}
	truncated := false
	size := 0
	for rr.NextRow() {
		if truncated || len(rows) >= caps.rows {
			truncated = true
			continue
		}
		raw := rr.Values()
		for _, v := range raw {
			size += len(v)
		}
		if size > caps.bytes {
			truncated = true
			continue
		}
		row := make([]any, len(raw))
		for i, v := range raw {
			row[i] = postgresValue(fields[i].DataTypeOID, v)
		}
		rows = append(rows, row)
	}
	return columns, rows, truncated
}

// editorConfig is the driver settings of the editor's connection: the
// connection's host, port, database and credentials, its TLS verified
// against the editor's CAs (sslMode verify-ca checks the chain only, any
// other mode the host name too: disable and prefer are not honored), SCRAM
// the only authentication accepted (a server asking for a clear-text or
// MD5 password gets nothing), the socket dialled through the network guard.
func (s *DefaultSqlService) editorConfig(values connectionValues, write bool) (*pgx.ConnConfig, error) {
	config, err := postgresConfig(values)
	if err != nil {
		return nil, err
	}
	config.Fallbacks = nil
	config.TLSConfig = verifiedTLS(config.Host, s.rootCAs, values.String("sslMode") == "verify-ca")
	config.RequireAuth = "scram-sha-256"
	config.ConnectTimeout = testTimeout
	config.DialFunc = s.guard.Dialer(testTimeout).DialContext
	config.RuntimeParams["application_name"] = "okdp-sql-editor"
	config.RuntimeParams["statement_timeout"] = fmt.Sprint(s.limits.StatementTimeout.Milliseconds())
	if !write {
		config.RuntimeParams["default_transaction_read_only"] = "on"
	}
	return config, nil
}

// verifiedTLS is a TLS client configuration verifying the server's
// certificate against roots (nil: the system's) and, unless chainOnly, its
// host name.
func verifiedTLS(host string, roots *x509.CertPool, chainOnly bool) *tls.Config {
	config := &tls.Config{ServerName: host, RootCAs: roots, MinVersion: tls.VersionTLS12}
	if !chainOnly {
		return config
	}
	// verify-ca: the chain, not the name. VerifyConnection runs with
	// InsecureSkipVerify, which only turns off Go's own check of both.
	config.InsecureSkipVerify = true // #nosec G402 -- the chain is verified below
	config.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("the server presented no certificate")
		}
		intermediates := x509.NewCertPool()
		for _, c := range cs.PeerCertificates[1:] {
			intermediates.AddCert(c)
		}
		_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates})
		return err
	}
	return config
}

// Errors of the editor's connection the console can tell apart.
var (
	// ErrSqlTLS: the server's certificate is not verified, or it offers no
	// TLS.
	ErrSqlTLS = errors.New("the database server's certificate could not be verified against the platform's CA bundle")
	// ErrSqlAuthMethod: the server asks for a password in clear text or
	// MD5, which the editor never sends.
	ErrSqlAuthMethod = errors.New("the database server asks for a clear-text or MD5 password: the SQL editor only authenticates with SCRAM-SHA-256")
)

// editorConnectError names a failed connection of the editor: an address
// the guard refuses, a certificate not verified, an authentication method
// refused, else the driver's error.
func editorConnectError(err error) error {
	var certErr *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	switch {
	case errors.Is(err, netguard.ErrRefused):
		return err
	case errors.As(err, &certErr), errors.As(err, &unknown), errors.As(err, &hostErr),
		strings.Contains(err.Error(), "server refused TLS connection"):
		return ErrSqlTLS
	case strings.Contains(err.Error(), "require_auth"), strings.Contains(err.Error(), "authentication method"):
		return ErrSqlAuthMethod
	}
	return err
}

// postgresFailure turns an execution error into the result the editor shows
// (SQL errors, timeout) or a proxy error (the connection broke).
func (s *DefaultSqlService) postgresFailure(ctx context.Context, query string, err error) (*models.SqlQueryResult, error) {
	empty := &models.SqlQueryResult{Columns: []models.SqlColumn{}, Rows: [][]any{}}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		empty.Error = &models.SqlQueryError{Message: pgErr.Message, ErrorName: pgErr.Code}
		if pgErr.Detail != "" {
			empty.Error.Message += ": " + pgErr.Detail
		}
		if pgErr.Position > 0 {
			empty.Error.LineNumber, empty.Error.ColumnNumber = lineColumn(query, int(pgErr.Position))
		}
		return empty, nil
	}
	if ctx.Err() != nil {
		empty.Error = &models.SqlQueryError{
			Message:   fmt.Sprintf("The query did not finish within %s and was cancelled.", s.limits.StatementTimeout),
			ErrorName: "TIMEOUT",
		}
		return empty, nil
	}
	return nil, fmt.Errorf("engine connection lost: %w", err)
}

// lineColumn converts PostgreSQL's 1-based character position into a 1-based
// line and column.
func lineColumn(query string, position int) (int, int) {
	line, column := 1, 1
	for i, r := range []rune(query) {
		if i >= position-1 {
			break
		}
		if r == '\n' {
			line, column = line+1, 1
		} else {
			column++
		}
	}
	return line, column
}

func postgresTypeName(m *pgtype.Map, oid uint32) string {
	if t, ok := m.TypeForOID(oid); ok {
		return t.Name
	}
	return "oid:" + strconv.FormatUint(uint64(oid), 10)
}

// maxSafeInteger is the largest integer a browser's float64 holds exactly.
const maxSafeInteger = 1<<53 - 1

// postgresValue converts one text-format value to JSON. Numbers stay numbers
// only when a browser reads them back exactly; numeric, large int8, NaN and
// infinities come back as their text. json/jsonb are passed through as JSON.
// Every other type (dates, intervals, arrays, bytea as \x…) is its PostgreSQL
// text form.
func postgresValue(oid uint32, raw []byte) any {
	if raw == nil {
		return nil
	}
	text := string(raw)
	switch oid {
	case pgtype.BoolOID:
		return text == "t"
	case pgtype.Int2OID, pgtype.Int4OID, pgtype.Int8OID, pgtype.OIDOID:
		if n, err := strconv.ParseInt(text, 10, 64); err == nil && n <= maxSafeInteger && n >= -maxSafeInteger {
			return n
		}
	case pgtype.Float4OID, pgtype.Float8OID:
		if f, err := strconv.ParseFloat(text, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f
		}
	case pgtype.JSONOID, pgtype.JSONBOID:
		if json.Valid(raw) {
			return json.RawMessage(text)
		}
	}
	return text
}
