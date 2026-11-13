package models

// SqlQueryRequest is a SQL statement to execute on a deployed SQL engine.
type SqlQueryRequest struct {
	Query string `json:"query" binding:"required"`
	// MaxRows caps the number of result rows returned (default 1000,
	// SQL_DEFAULT_ROWS; at most SQL_MAX_ROWS, 10000 by default).
	MaxRows int `json:"maxRows,omitempty"`
	// Write runs a connection's statement in write mode: a batch of
	// statements separated by ';', in one implicit transaction. Without
	// it, one statement in a read-only transaction. Not read for a
	// service instance (Trino authorizes the caller's own token).
	Write bool `json:"write,omitempty"`
}

// SqlColumn describes one column of a SQL result set.
type SqlColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// SqlQueryError is a query failure reported by the engine (syntax error,
// missing table…, or a PostgreSQL statement past the proxy timeout) — the
// proxy call itself succeeded. ErrorName is Trino's error name or
// PostgreSQL's SQLSTATE.
type SqlQueryError struct {
	Message      string `json:"message"`
	ErrorName    string `json:"errorName,omitempty"`
	LineNumber   int    `json:"lineNumber,omitempty"`
	ColumnNumber int    `json:"columnNumber,omitempty"`
}

// Which cap truncated a result (SqlQueryResult.TruncatedBy).
const (
	SqlTruncatedByRows  = "rows"
	SqlTruncatedByBytes = "bytes"
)

// SqlQueryResult is the outcome of a SQL execution: either a result set or
// an engine-reported error.
type SqlQueryResult struct {
	QueryID   string      `json:"queryId,omitempty"`
	Columns   []SqlColumn `json:"columns"`
	Rows      [][]any     `json:"rows"`
	RowCount  int         `json:"rowCount"`
	Truncated bool        `json:"truncated"`
	// TruncatedBy says which cap dropped rows when Truncated: rows (the
	// row cap, MaxRows) or bytes (SQL_MAX_RESULT_BYTES).
	TruncatedBy string `json:"truncatedBy,omitempty"`
	// MaxRows is the row cap the statement ran with: the request's
	// maxRows, else SQL_DEFAULT_ROWS, at most SQL_MAX_ROWS.
	MaxRows   int   `json:"maxRows"`
	ElapsedMs int64 `json:"elapsedMs"`
	// CommandTag is the PostgreSQL command tag of the last statement
	// (e.g. "INSERT 0 5"). Empty for Trino.
	CommandTag string         `json:"commandTag,omitempty"`
	Error      *SqlQueryError `json:"error,omitempty"`
}
