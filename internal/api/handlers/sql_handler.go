package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/netguard"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

type SqlHandler struct {
	service service.SqlService
}

func NewSqlHandler(svc service.SqlService) *SqlHandler {
	return &SqlHandler{service: svc}
}

// Error codes of the SQL routes (the `code` of an error answer).
const (
	ErrorCodeSqlCredentialsRefused = "sql-credentials-refused"
	ErrorCodeSqlClaimUnbound       = "sql-claim-unbound"
	ErrorCodeSqlBusy               = "sql-busy"
	ErrorCodeSqlWriteRefused       = "sql-write-refused"
	ErrorCodeSqlAddressRefused     = "sql-address-refused"
	ErrorCodeSqlTLS                = "sql-tls-unverified"
	ErrorCodeSqlAuthMethod         = "sql-auth-method-refused"
	ErrorCodeSqlUnsupported        = "sql-unsupported"
	ErrorCodeSqlNoURL              = "sql-no-url"
	ErrorCodeSqlNoCredentials      = "sql-no-credentials"
	ErrorCodeSqlTimeout            = "sql-timeout"
)

// ExecuteQuery godoc
// @Summary      Execute a SQL query on a deployed SQL engine
// @Description  Proxies the statement to the service instance (Trino only), forwarding the caller's bearer token; `write` is not read (the engine authorizes the token). Engine-side SQL errors answer 200 with `error` set. Every statement is logged with the caller and the instance. Bounded by the statement timeout (SQL_STATEMENT_TIMEOUT: 504, code sql-timeout, the query cancelled), the row cap (maxRows, at most SQL_MAX_ROWS; the result's maxRows is the cap applied) and the result-size cap (SQL_MAX_RESULT_BYTES), past which the result is truncated (truncatedBy: rows or bytes); a client that disconnects cancels the query. A viewer of the project may run it. 429 (code sql-busy): the caller already runs SQL_MAX_CONCURRENT_PER_USER statements. 400 (code sql-address-refused): the engine address is loopback, link-local or the Kubernetes API server; 400 (code sql-unsupported): the instance is no Trino; 400 (code sql-no-url): it publishes no URL yet. Any other failure of the engine answers 502 with a generic message and the requestId, its cause in the server's log.
// @Tags         sql
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name"
// @Param        serviceName path string true "Service instance name"
// @Param        request body models.SqlQueryRequest true "SQL Query Request"
// @Success      200  {object}  models.SqlQueryResult
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      429  {object}  map[string]string
// @Failure      502  {object}  map[string]string
// @Failure      504  {object}  map[string]string
// @Router       /api/projects/{name}/services/{serviceName}/sql [post]
func (h *SqlHandler) ExecuteQuery(c *gin.Context) {
	req, ok := bindSqlRequest(c)
	if !ok {
		return
	}
	result, err := h.service.ExecuteQuery(c.Request.Context(), c.Param("name"), c.Param("serviceName"), c.GetHeader("Authorization"), req)
	respondSql(c, result, err)
}

// ExecuteOnConnection godoc
// @Summary      Execute SQL on a database connection
// @Description  Runs a statement on a database-server connection (engine postgresql) declared in the tree: the project's, else the platform's, else a named claim of the project (the connection its provider granted). Host, port and database are the connection's, never the request's. The credentials come only from the claim's provisioned Secret (labelled okdp.io/credentials-database-server), the platform's secret store (secretRef.remoteKey, the Secret an ExternalSecret of the project fills from it), the Secret the console wrote from the connection form, or a Secret labelled okdp.io/connection-credentials: "true"; any other answers 400 (code sql-credentials-refused). Every project member acts as the connection's database user. TLS is always verified against the platform CA bundle (sslMode verify-ca checks the chain only; disable and prefer are not honored): 502 (code sql-tls-unverified) otherwise. Only SCRAM-SHA-256 authentication: a server asking for a clear-text or MD5 password answers 502 (code sql-auth-method-refused). Loopback, link-local and Kubernetes API server addresses answer 400 (code sql-address-refused) unless SQL_ALLOWED_NETWORKS allows them. Read-only by default: one statement, in a READ ONLY transaction rolled back after it (a batch of several is an SQL error, 42601). With `write: true`, a batch: several statements separated by ';' run in one implicit transaction; the result is the last statement that returned columns, with the command tag of the last statement. Every statement is logged with the caller and the connection. Rows past maxRows (at most SQL_MAX_ROWS; the result's maxRows is the cap applied) or SQL_MAX_RESULT_BYTES are dropped (not cancelled), truncatedBy rows or bytes. SQL errors (errorName = SQLSTATE) and the statement timeout (SQL_STATEMENT_TIMEOUT, 2 minutes) answer 200 with `error` set. A project editor's: it runs with credentials of the project's Secrets, which a viewer cannot read (403, code forbidden). 429 (code sql-busy): the caller already runs SQL_MAX_CONCURRENT_PER_USER statements. 400 (code sql-claim-unbound): a named claim no provider granted yet; 400 (code sql-unsupported): the connection is no database-server of engine postgresql (the sql flag of GET .../connections/selectable and .../claims); 400 (code sql-no-credentials): its credentials Secret does not exist yet. Any other failure of the database answers 502 with a generic message and the requestId, its cause in the server's log.
// @Tags         sql
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name"
// @Param        connName path string true "Connection name"
// @Param        request body models.SqlQueryRequest true "SQL Query Request"
// @Success      200  {object}  models.SqlQueryResult
// @Failure      400  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      429  {object}  map[string]string
// @Failure      502  {object}  map[string]string
// @Router       /api/projects/{name}/connections/{connName}/sql [post]
func (h *SqlHandler) ExecuteOnConnection(c *gin.Context) {
	req, ok := bindSqlRequest(c)
	if !ok {
		return
	}
	result, err := h.service.ExecuteOnConnection(c.Request.Context(), c.Param("name"), c.Param("connName"), req)
	respondSql(c, result, err)
}

func bindSqlRequest(c *gin.Context) (models.SqlQueryRequest, bool) {
	var req models.SqlQueryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return req, false
	}
	if strings.TrimSpace(req.Query) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query must not be empty"})
		return req, false
	}
	return req, true
}

func respondSql(c *gin.Context, result *models.SqlQueryResult, err error) {
	if err != nil {
		coded := func(status int, code string) {
			c.JSON(status, gin.H{"error": err.Error(), "code": code})
		}
		switch {
		case apierrors.IsNotFound(err):
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		case errors.Is(err, service.ErrSqlCredentialsRefused):
			coded(http.StatusBadRequest, ErrorCodeSqlCredentialsRefused)
		case errors.Is(err, service.ErrSqlUnbound):
			coded(http.StatusBadRequest, ErrorCodeSqlClaimUnbound)
		case errors.Is(err, netguard.ErrRefused):
			coded(http.StatusBadRequest, ErrorCodeSqlAddressRefused)
		case errors.Is(err, service.ErrSqlBusy):
			coded(http.StatusTooManyRequests, ErrorCodeSqlBusy)
		case errors.Is(err, service.ErrSqlWriteRefused):
			coded(http.StatusForbidden, ErrorCodeSqlWriteRefused)
		case errors.Is(err, service.ErrSqlTLS):
			coded(http.StatusBadGateway, ErrorCodeSqlTLS)
		case errors.Is(err, service.ErrSqlAuthMethod):
			coded(http.StatusBadGateway, ErrorCodeSqlAuthMethod)
		case errors.Is(err, service.ErrSqlUnsupported):
			coded(http.StatusBadRequest, ErrorCodeSqlUnsupported)
		case errors.Is(err, service.ErrSqlNoURL):
			coded(http.StatusBadRequest, ErrorCodeSqlNoURL)
		case errors.Is(err, service.ErrSqlNoCredentials):
			coded(http.StatusBadRequest, ErrorCodeSqlNoCredentials)
		case errors.Is(err, service.ErrSqlTimeout):
			coded(http.StatusGatewayTimeout, ErrorCodeSqlTimeout)
		case errors.Is(err, service.ErrConnectionsUnavailable):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			// The engine's or the driver's text stays in the log.
			writeInternal(c, http.StatusBadGateway, err, "Failed to execute SQL query")
		}
		return
	}
	c.JSON(http.StatusOK, result)
}
