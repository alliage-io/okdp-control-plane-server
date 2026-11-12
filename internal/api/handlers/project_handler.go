package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// ProjectHandler handles project-related requests
type ProjectHandler struct {
	service service.ProjectService
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
// @Failure      500  {object}  map[string]string
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
// @Failure      404  {object}  map[string]string "Project not found"
// @Failure      500  {object}  map[string]string "Internal server error"
// @Router       /api/projects/{name} [get]
func (h *ProjectHandler) GetProject(c *gin.Context) {
	name := c.Param("name")
	project, err := h.service.GetProject(c.Request.Context(), name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
			return
		}
		writeError(c, err, "Failed to get project")
		return
	}

	c.JSON(http.StatusOK, project)
}

// CreateProject godoc
// @Summary      Create a project
// @Description  Create a project: commits projects/<name>/project.yaml (in a split topology, to a new repository of its own, created through the Forgejo API with project.yaml only: no workflow, no secret, the CI of the output repository compiles it) and creates the project Namespace. 409 when the project is already declared, or when a Namespace of that name exists and is not a project's
// @Tags         projects
// @Accept       json
// @Produce      json
// @Param        project body models.Project true "Project Object"
// @Success      201  {object}  models.Project
// @Failure      400  {object}  map[string]string "Invalid project name"
// @Failure      409  {object}  map[string]string "Project or Namespace already exists"
// @Failure      500  {object}  map[string]string
// @Router       /api/projects [post]
func (h *ProjectHandler) CreateProject(c *gin.Context) {
	var project models.Project
	if err := c.ShouldBindJSON(&project); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.service.CreateProject(c.Request.Context(), &project); err != nil {
		switch {
		case apierrors.IsBadRequest(err):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		case apierrors.IsAlreadyExists(err):
			c.JSON(http.StatusConflict, gin.H{"error": "Project '" + project.Name + "' already exists, or its namespace exists and is not a project's"})
			return
		}
		logrus.Errorf("Failed to create project: %v", err)
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
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string "Project not found"
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name} [put]
func (h *ProjectHandler) UpdateProject(c *gin.Context) {
	var project models.Project
	if err := c.ShouldBindJSON(&project); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// The path name is authoritative; ignore any name in the body.
	project.Name = c.Param("name")

	updated, err := h.service.UpdateProject(c.Request.Context(), &project)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
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
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/{name} [delete]
func (h *ProjectHandler) DeleteProject(c *gin.Context) {
	name := c.Param("name")
	if err := h.service.DeleteProject(c.Request.Context(), name); err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
			return
		}
		writeError(c, err, "Failed to delete project")
		return
	}
	c.Status(http.StatusNoContent)
}

// StreamProjects godoc
// @Summary      Stream project updates
// @Description  Stream project updates using Server-Sent Events (SSE): an ADDED event per existing project the caller may see, then ADDED/MODIFIED/DELETED as project.yaml files change in the deployments repository (read every few seconds, at once after a console change); a project the caller stops seeing (its access changed) is DELETED from its stream
// @Tags         projects
// @Produce      text/event-stream
// @Success      200  {string}  string  "stream"
// @Failure      500  {object}  map[string]string
// @Router       /api/projects/stream [get]
func (h *ProjectHandler) StreamProjects(c *gin.Context) {
	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Transfer-Encoding", "chunked")

	events, err := h.service.WatchProjects(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}

	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	c.Writer.Flush()

	// The projects this stream showed: a DELETED is sent of those only, and
	// one the caller stops seeing is DELETED.
	shown := map[string]bool{}
	ctx := c.Request.Context()

	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-keepalive.C:
			// A comment line keeps proxies from cutting an idle stream.
			if _, err := c.Writer.WriteString(": keepalive\n\n"); err != nil {
				return
			}
			c.Writer.Flush()
		case event, ok := <-events:
			if !ok {
				return
			}
			name := event.Object.Name
			switch {
			case event.Type == "DELETED" && !shown[name]:
				continue
			case event.Type == "DELETED":
				delete(shown, name)
			case canView(ctx, name):
				shown[name] = true
			case shown[name]:
				delete(shown, name)
				event.Type = "DELETED"
			default:
				continue
			}
			c.SSEvent("message", event)
			c.Writer.Flush()
		}
	}
}

// Resolve looks a project up by name. It is what middleware.RequireProject
// calls to establish that a path segment really designates a project, before
// any handler passes it to the Kubernetes client as a namespace.
func (h *ProjectHandler) Resolve(c *gin.Context, name string) (*models.Project, error) {
	return h.service.GetProject(c.Request.Context(), name)
}
