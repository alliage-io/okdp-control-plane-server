package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

// ClaimHandler handles the claims of a project: its named claims, the
// classes a claim may be of, and the claims of an instance.
type ClaimHandler struct {
	service service.ClaimService
}

// NewClaimHandler creates a new ClaimHandler.
func NewClaimHandler(service service.ClaimService) *ClaimHandler {
	return &ClaimHandler{service: service}
}

// ListNamedClaims godoc
// @Summary      List the named claims of a project
// @Description  The named claims of the project (claims.<name> of projects/<p>/project.yaml): each a connection of its contract, declared once, that every instance of the project naming it shares; with the provider the compile of the tree binds it to (its class, or the provider it names), the instances that name it, and why it does not bind; the engine of the connection its provider granted, and whether the SQL editor runs on it (sql)
// @Tags         claims
// @Produce      json
// @Param        name path string true "Project name (= Kubernetes namespace)"
// @Success      200  {array}   models.NamedClaimView
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      404  {object}  models.ErrorResponse
// @Failure      500  {object}  models.ErrorResponse
// @Router       /api/projects/{name}/claims [get]
func (h *ClaimHandler) ListNamedClaims(c *gin.Context) {
	claims, err := h.service.ListNamedClaims(c.Request.Context(), projectNamespace(c))
	if err != nil {
		h.writeError(c, err, "Failed to list the named claims")
		return
	}
	c.JSON(http.StatusOK, claims)
}

// CreateNamedClaim godoc
// @Summary      Declare a named claim
// @Description  Commits claims.<name> to the project's project.yaml (its comments and other keys kept): a contract, a class of the project or the platform or a provider (<instance>, <release>, <namespace>/<instance> or <namespace>/<release>), and a request the contract's claim schema allows (x-okdp-claim). Checked first (the class, the request), then by the compile of the tree: refused (400, code does-not-compile) when an instance that compiled no longer does
// @Tags         claims
// @Accept       json
// @Produce      json
// @Param        name  path string                   true "Project name"
// @Param        claim body models.NamedClaimRequest true "The claim, its name included"
// @Success      201  {object}  models.NamedClaimView
// @Failure      400  {object}  models.ErrorResponse
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      404  {object}  models.ErrorResponse "Project not found"
// @Failure      409  {object}  models.ErrorResponse "A named claim of that name exists"
// @Failure      413  {object}  models.ErrorResponse  "the request body is too large"
// @Failure      500  {object}  models.ErrorResponse
// @Router       /api/projects/{name}/claims [post]
func (h *ClaimHandler) CreateNamedClaim(c *gin.Context) {
	var req models.NamedClaimRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: err.Error()})
		return
	}
	claim, err := h.service.CreateNamedClaim(c.Request.Context(), projectNamespace(c), req)
	if err != nil {
		h.writeError(c, err, "Failed to declare a named claim")
		return
	}
	c.JSON(http.StatusCreated, claim)
}

// UpdateNamedClaim godoc
// @Summary      Change a named claim
// @Description  Replaces the declaration of claims.<claimName> in the project's project.yaml, checked as a creation is. A changed class or provider moves the claim to a new, empty connection: its former provider keeps what it held
// @Tags         claims
// @Accept       json
// @Produce      json
// @Param        name      path string                   true "Project name"
// @Param        claimName path string                   true "Claim name"
// @Param        claim     body models.NamedClaimRequest true "The claim"
// @Success      200  {object}  models.NamedClaimView
// @Failure      400  {object}  models.ErrorResponse
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      404  {object}  models.ErrorResponse
// @Failure      413  {object}  models.ErrorResponse  "the request body is too large"
// @Failure      500  {object}  models.ErrorResponse
// @Router       /api/projects/{name}/claims/{claimName} [put]
func (h *ClaimHandler) UpdateNamedClaim(c *gin.Context) {
	var req models.NamedClaimRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: err.Error()})
		return
	}
	claim, err := h.service.UpdateNamedClaim(c.Request.Context(), projectNamespace(c), c.Param("claimName"), req)
	if err != nil {
		h.writeError(c, err, "Failed to change a named claim")
		return
	}
	c.JSON(http.StatusOK, claim)
}

// DeleteNamedClaim godoc
// @Summary      Remove a named claim
// @Description  Removes claims.<claimName> from the project's project.yaml; refused (400) while an instance of the project names it. Its provider keeps what the claim's connection holds (a database, a bucket)
// @Tags         claims
// @Param        name      path string true "Project name"
// @Param        claimName path string true "Claim name"
// @Success      204
// @Failure      400  {object}  models.ErrorResponse
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      404  {object}  models.ErrorResponse
// @Failure      500  {object}  models.ErrorResponse
// @Router       /api/projects/{name}/claims/{claimName} [delete]
func (h *ClaimHandler) DeleteNamedClaim(c *gin.Context) {
	if err := h.service.DeleteNamedClaim(c.Request.Context(), projectNamespace(c), c.Param("claimName")); err != nil {
		h.writeError(c, err, "Failed to remove a named claim")
		return
	}
	c.Status(http.StatusNoContent)
}

// GetProvisioning godoc
// @Summary      List the classes of the claims
// @Description  The classes a claim of the project may be of, by contract: the platform's (global.okdp.provisioning of platform.yaml) and the project's (of its project.yaml), each a provider and what it fixes of the request, the default class, and the contract's claim schema (x-okdp-claim): what a claim may request
// @Tags         claims
// @Produce      json
// @Param        name path string true "Project name"
// @Success      200  {object}  models.ProvisioningResponse
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      404  {object}  models.ErrorResponse
// @Failure      500  {object}  models.ErrorResponse
// @Router       /api/projects/{name}/provisioning [get]
func (h *ClaimHandler) GetProvisioning(c *gin.Context) {
	out, err := h.service.Provisioning(c.Request.Context(), projectNamespace(c))
	if err != nil {
		h.writeError(c, err, "Failed to list the classes of the claims")
		return
	}
	c.JSON(http.StatusOK, out)
}

// GetInstanceClaims godoc
// @Summary      List the claims of an instance
// @Description  The claims of the instance's connection references, each bound to its provider (the provider of its class, or the one it names) and what the provider granted; and, when its package provisions contracts (okdp.yaml provisions), the claims bound to it, their claimers, and what it granted each (the connection it publishes for the claim, its okdp.info.yaml). From the compile of the tree: what the next deployment binds
// @Tags         claims
// @Produce      json
// @Param        name        path string true "Project name"
// @Param        serviceName path string true "Instance name"
// @Success      200  {object}  models.InstanceClaims
// @Failure      400  {object}  models.ErrorResponse  "the request is refused (code, fields when the compile refuses it)"
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      404  {object}  models.ErrorResponse
// @Failure      500  {object}  models.ErrorResponse
// @Router       /api/projects/{name}/services/{serviceName}/claims [get]
func (h *ClaimHandler) GetInstanceClaims(c *gin.Context) {
	out, err := h.service.InstanceClaims(c.Request.Context(), projectNamespace(c), c.Param("serviceName"))
	if err != nil {
		h.writeError(c, err, "Failed to list the claims of an instance")
		return
	}
	c.JSON(http.StatusOK, out)
}

// writeError maps a service error to its status: a missing project, claim
// or instance (404), a claim of that name (409), rejected input (400, the
// compiler's message for a compile that fails), anything else (500).
func (h *ClaimHandler) writeError(c *gin.Context, err error, logMessage string) {
	writeError(c, err, logMessage)
}
