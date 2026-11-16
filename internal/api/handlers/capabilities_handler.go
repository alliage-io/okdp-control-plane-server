package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

type CapabilitiesHandler struct {
	service service.CapabilityService
}

func NewCapabilitiesHandler(service service.CapabilityService) *CapabilitiesHandler {
	return &CapabilitiesHandler{service: service}
}

// GetCapabilities godoc
// @Summary      Get platform capabilities
// @Description  Capabilities the platform is configured with (user management through Keycloak, console OIDC client, OIDC client provisioning backend), so the UI can adapt. Public: what the caller may do, and the bounds of the SQL editor (sql, no longer sent here), are GET /api/me
// @Tags         capabilities
// @Produce      json
// @Success      200  {object}  models.Capabilities
// @Router       /api/capabilities [get]
func (h *CapabilitiesHandler) GetCapabilities(c *gin.Context) {
	caps, err := h.service.GetCapabilities(c.Request.Context())
	if err != nil {
		writeError(c, err, "Failed to resolve platform capabilities")
		return
	}
	c.JSON(http.StatusOK, caps)
}
