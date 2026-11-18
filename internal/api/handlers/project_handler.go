package handlers

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/okdp/okdp-control-plane-server/internal/apperr"
	"github.com/okdp/okdp-control-plane-server/internal/authz"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

// ProjectHandler handles project-related requests
type ProjectHandler struct {
	service service.ProjectService
	// Sql are the bounds of the SQL editor, sql of GET /api/me.
	Sql models.SqlCapability
}

// NewProjectHandler creates a new ProjectHandler
func NewProjectHandler(service service.ProjectService) *ProjectHandler {
	return &ProjectHandler{
		service: service,
	}
}

// ListProjects godoc
// @Summary      List the caller's projects
// @Description  List the projects declared in the deployments repository (projects/<p>/project.yaml), whether written by the console or by hand in Git, that the caller may see: those it has a role in (project.yaml access, okdp-project-<p> an editor by default), every one for a platform admin
// @Tags         projects
// @Accept       json
// @Produce      json
// @Success      200  {array}   models.Project
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      500  {object}  models.ErrorResponse
// @Router       /api/projects [get]
func (h *ProjectHandler) ListProjects(c *gin.Context) {
	projects, err := h.service.ListProjects(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, visibleProjects(c.Request.Context(), projects))
}

// GetProject godoc
// @Summary      Get a project
// @Description  Get a project declared in the deployments repository (projects/<name>/project.yaml)
// @Tags         projects
// @Accept       json
// @Produce      json
// @Param        name path string true "Project Name"
// @Success      200  {object}  models.Project
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      404  {object}  models.ErrorResponse "Project not found"
// @Failure      500  {object}  models.ErrorResponse "Internal server error"
// @Router       /api/projects/{name} [get]
func (h *ProjectHandler) GetProject(c *gin.Context) {
	name := c.Param("name")
	project, err := h.service.GetProject(c.Request.Context(), name)
	if err != nil {
		if errors.Is(err, apperr.ErrNotFound) {
			c.JSON(http.StatusNotFound, models.ErrorResponse{Error: "Project not found"})
			return
		}
		writeError(c, err, "Failed to get project")
		return
	}

	c.JSON(http.StatusOK, project)
}

// CreateProject godoc
// @Summary      Create a project
// @Description  Create a project: commits projects/<name>/project.yaml (in a split topology, to a new repository of its own, created through the Forgejo API with project.yaml only: no workflow, no secret, the CI of the output repository compiles it) and creates the project Namespace. 409 when the project is already declared, or when a Namespace of that name exists and is not a project's, or when the project's repository exists in the Forgejo organisation and is not empty
// @Tags         projects
// @Accept       json
// @Produce      json
// @Param        project body models.Project true "Project Object"
// @Success      201  {object}  models.Project
// @Failure      400  {object}  models.ErrorResponse "Invalid project name"
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      409  {object}  models.ErrorResponse "Project or Namespace already exists"
// @Failure      413  {object}  models.ErrorResponse  "the request body is too large"
// @Failure      500  {object}  models.ErrorResponse
// @Router       /api/projects [post]
func (h *ProjectHandler) CreateProject(c *gin.Context) {
	var project models.Project
	if err := c.ShouldBindJSON(&project); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: err.Error()})
		return
	}

	if err := h.service.CreateProject(c.Request.Context(), &project); err != nil {
		switch {
		case errors.Is(err, apperr.ErrInvalid):
			c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: err.Error()})
			return
		case errors.Is(err, apperr.ErrAlreadyExists):
			c.JSON(http.StatusConflict, models.ErrorResponse{Error: "Project '" + project.Name + "' already exists, or its namespace exists and is not a project's"})
			return
		}
		slog.Error(fmt.Sprintf("Failed to create project: %v", err))
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, project)
}

// UpdateProject godoc
// @Summary      Update a project
// @Description  Update a project's mutable metadata (currently its description) in projects/<name>/project.yaml; the file's other keys are kept
// @Tags         projects
// @Accept       json
// @Produce      json
// @Param        name    path string         true "Project Name"
// @Param        project body models.Project true "Project Object"
// @Success      200  {object}  models.Project
// @Failure      400  {object}  models.ErrorResponse
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      404  {object}  models.ErrorResponse "Project not found"
// @Failure      413  {object}  models.ErrorResponse  "the request body is too large"
// @Failure      500  {object}  models.ErrorResponse
// @Router       /api/projects/{name} [put]
func (h *ProjectHandler) UpdateProject(c *gin.Context) {
	var project models.Project
	if err := c.ShouldBindJSON(&project); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: err.Error()})
		return
	}
	// The path name is authoritative; ignore any name in the body.
	project.Name = c.Param("name")

	updated, err := h.service.UpdateProject(c.Request.Context(), &project)
	if err != nil {
		if errors.Is(err, apperr.ErrNotFound) {
			c.JSON(http.StatusNotFound, models.ErrorResponse{Error: "Project not found"})
			return
		}
		writeError(c, err, "Failed to update project")
		return
	}

	c.JSON(http.StatusOK, updated)
}

// DeleteProject godoc
// @Summary      Delete a project
// @Description  Delete a project: removes projects/<name>/ from the deployments repository (the compile of that commit, okdp-gitops in CI, removes its compiled tree, and the GitOps engine uninstalls its releases), then its Namespace if the console created it
// @Tags         projects
// @Accept       json
// @Produce      json
// @Param        name path string true "Project Name"
// @Success      204  {object}  nil
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      404  {object}  models.ErrorResponse
// @Failure      500  {object}  models.ErrorResponse
// @Router       /api/projects/{name} [delete]
func (h *ProjectHandler) DeleteProject(c *gin.Context) {
	name := c.Param("name")
	if err := h.service.DeleteProject(c.Request.Context(), name); err != nil {
		if errors.Is(err, apperr.ErrNotFound) {
			c.JSON(http.StatusNotFound, models.ErrorResponse{Error: "Project not found"})
			return
		}
		writeError(c, err, "Failed to delete project")
		return
	}
	c.Status(http.StatusNoContent)
}

// StreamProjects godoc
// @Summary      Stream project updates
// @Description  Stream project updates using Server-Sent Events (SSE): an ADDED event per existing project the caller may see, then ADDED/MODIFIED/DELETED as project.yaml files change in the deployments repository (read every few seconds, at once after a console change). A change of its description or of its access alone (project.yaml access) is MODIFIED: the console reads GET /api/me again on any event. A project the caller starts seeing (its access changed) is ADDED to its stream, one it stops seeing DELETED
// @Tags         projects
// @Produce      text/event-stream
// @Success      200  {string}  string  "stream"
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      500  {object}  models.ErrorResponse
// @Router       /api/projects/stream [get]
func (h *ProjectHandler) StreamProjects(c *gin.Context) {
	events, err := h.service.WatchProjects(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}

	// The projects this stream showed: a DELETED is sent of those only, and
	// one the caller stops seeing is DELETED.
	shown := map[string]bool{}

	serveStream(c, events, func(s *sseStream, event service.ProjectEvent) error {
		ctx := s.Context()
		name := event.Object.Name
		if event.Type == "MODIFIED" {
			// Its access may be what changed: the caller's role read
			// from its project.yaml as it is now, not as kept.
			authz.CallerFrom(ctx).Forget(name)
		}
		switch {
		case event.Type == "DELETED" && !shown[name]:
			return nil
		case event.Type == "DELETED":
			delete(shown, name)
		case canView(ctx, name):
			if !shown[name] {
				// A project the caller starts seeing is new to it.
				event.Type = "ADDED"
			}
			shown[name] = true
		case shown[name]:
			delete(shown, name)
			event.Type = "DELETED"
		default:
			return nil
		}
		return s.Event("message", event)
	})
}

// Resolve looks a project up by name. It is what middleware.RequireProject
// calls to establish that a path segment really designates a project, before
// any handler passes it to the Kubernetes client as a namespace.
func (h *ProjectHandler) Resolve(c *gin.Context, name string) (*models.Project, error) {
	return h.service.GetProject(c.Request.Context(), name)
}
