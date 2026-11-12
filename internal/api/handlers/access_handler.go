package handlers

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/authz"
	"github.com/okdp/okdp-control-plane-server/internal/models"
)

// GetCallerAccess godoc
// @Summary      What the caller may do
// @Description  The caller's rights, read as every route reads them: whether it is a platform admin (a group of platform.yaml global.okdp.access.admins: identity, catalog writes, project creation and deletion) and its role in each project (project.yaml access, okdp-project-<p> an editor by default; a viewer reads, an editor writes, an admin edits project.yaml; a platform admin is admin everywhere). A project absent from projects is one the caller may not see. A route the caller may not use answers 403 {"error", "code": "forbidden"}
// @Tags         access
// @Produce      json
// @Success      200  {object}  models.CallerAccess
// @Failure      500  {object}  map[string]string
// @Router       /api/me [get]
func (h *ProjectHandler) GetCallerAccess(c *gin.Context) {
	ctx := c.Request.Context()
	caller := authz.CallerFrom(ctx)
	admin, err := caller.IsPlatformAdmin(ctx)
	if err != nil {
		writeError(c, err, "Failed to read the platform's authority")
		return
	}
	projects, err := h.service.ListProjects(ctx)
	if err != nil {
		writeError(c, err)
		return
	}
	roles := map[string]string{}
	for _, p := range projects {
		role, err := caller.ProjectRole(ctx, p.Name)
		if err != nil {
			writeError(c, err, "Failed to read the platform's authority")
			return
		}
		if role != "" {
			roles[p.Name] = string(role)
		}
	}
	groups := caller.Groups()
	if groups == nil {
		groups = []string{}
	}
	c.JSON(http.StatusOK, models.CallerAccess{Username: auth.ActorName(ctx), Groups: groups, PlatformAdmin: admin, Projects: roles})
}

// canView reports whether the caller of ctx may see project: a viewer of
// it, or more. A caller the authority cannot be read for sees nothing.
func canView(ctx context.Context, project string) bool {
	ok, err := authz.CallerFrom(ctx).Allows(ctx, authz.ProjectViewer, project)
	return err == nil && ok
}

// visibleProjects keeps the projects the caller of ctx may see.
func visibleProjects(ctx context.Context, projects []models.Project) []models.Project {
	kept := make([]models.Project, 0, len(projects))
	for _, p := range projects {
		if canView(ctx, p.Name) {
			kept = append(kept, p)
		}
	}
	return kept
}
