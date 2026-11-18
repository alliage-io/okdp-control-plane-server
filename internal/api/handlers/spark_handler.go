package handlers

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/okdp/okdp-control-plane-server/internal/apperr"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

type SparkHandler struct {
	service service.SparkService
}

func NewSparkHandler(svc service.SparkService) *SparkHandler {
	return &SparkHandler{service: svc}
}

// SubmitSparkApp godoc
// @Summary      Submit a Spark application
// @Description  Builds a SparkApplication from the request and submits it to the Spark operator. Its driver and executors run as the ServiceAccount of the project's spark-rbac instance (400 when the project has none). type is Scala, Java, Python or R; mode cluster; image one the platform allows (global.okdp.sparkOperator.spark.images, or its image.registry; any when neither is set); sparkConf keys Spark's own settings only: no spark.kubernetes.* key but the driver's and executors' labels, resources and allocation, nothing naming volumes, a ServiceAccount, a pod template, a host path or a security context (400 otherwise).
// @Tags         spark-apps
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name"
// @Param        request body models.SparkAppRequest true "Spark Application Request"
// @Success      201  {object}  models.SparkAppInstance
// @Failure      400  {object}  models.ErrorResponse
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      404  {object}  models.ErrorResponse  "not found"
// @Failure      409  {object}  models.ErrorResponse
// @Failure      413  {object}  models.ErrorResponse  "the request body is too large"
// @Failure      500  {object}  models.ErrorResponse
// @Router       /api/projects/{name}/spark-apps [post]
func (h *SparkHandler) SubmitSparkApp(c *gin.Context) {
	project := c.Param("name")

	var req models.SparkAppRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: err.Error()})
		return
	}

	instance, err := h.service.SubmitApp(c.Request.Context(), project, req)
	if err != nil {
		if errors.Is(err, apperr.ErrAlreadyExists) {
			c.JSON(http.StatusConflict, models.ErrorResponse{
				Error: fmt.Sprintf("SparkApplication '%s' already exists in project '%s'", req.Name, project),
			})
			return
		}
		if service.IsValidationError(err) {
			c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: err.Error()})
			return
		}
		writeError(c, err, "Failed to submit Spark application")
		return
	}
	c.JSON(http.StatusCreated, instance)
}

// SubmitSparkAppYAML godoc
// @Summary      Submit a Spark application from YAML
// @Description  Reads a SparkApplication manifest into the request of POST /spark-apps and submits it the same way. Besides metadata.name (and metadata.namespace, the project's), it may hold the form's fields, imagePullPolicy, restartPolicy, deps and the driver's and executors' coreLimit; a serviceAccount other than the project's, or any other field (volumes, securityContext, podTemplate, nodeSelector...), answers 400.
// @Tags         spark-apps
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name"
// @Param        request body models.SparkAppYAMLRequest true "Spark Application YAML"
// @Success      201  {object}  models.SparkAppInstance
// @Failure      400  {object}  models.ErrorResponse
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      404  {object}  models.ErrorResponse  "not found"
// @Failure      409  {object}  models.ErrorResponse
// @Failure      413  {object}  models.ErrorResponse  "the request body is too large"
// @Failure      500  {object}  models.ErrorResponse
// @Router       /api/projects/{name}/spark-apps/yaml [post]
func (h *SparkHandler) SubmitSparkAppYAML(c *gin.Context) {
	project := c.Param("name")

	var req models.SparkAppYAMLRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: err.Error()})
		return
	}

	instance, err := h.service.SubmitAppYAML(c.Request.Context(), project, req)
	if err != nil {
		if errors.Is(err, apperr.ErrAlreadyExists) {
			c.JSON(http.StatusConflict, models.ErrorResponse{
				Error: fmt.Sprintf("SparkApplication already exists in project '%s'", project),
			})
			return
		}
		if service.IsValidationError(err) {
			c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: err.Error()})
			return
		}
		writeError(c, err, "Failed to submit Spark application from YAML")
		return
	}
	c.JSON(http.StatusCreated, instance)
}

// ListSparkApps godoc
// @Summary      List Spark applications for a project
// @Description  Get all SparkApplication instances for a project
// @Tags         spark-apps
// @Produce      json
// @Param        name path string true "Project name"
// @Success      200  {array}   models.SparkAppInstance
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      404  {object}  models.ErrorResponse  "not found"
// @Failure      500  {object}  models.ErrorResponse
// @Router       /api/projects/{name}/spark-apps [get]
func (h *SparkHandler) ListSparkApps(c *gin.Context) {
	project := c.Param("name")

	apps, err := h.service.ListApps(c.Request.Context(), project)
	if err != nil {
		writeError(c, err, "Failed to list Spark applications")
		return
	}
	c.JSON(http.StatusOK, apps)
}

// StreamSparkApps godoc
// @Summary      Stream Spark application updates
// @Description  Stream SparkApplication status updates using Server-Sent Events (SSE)
// @Tags         spark-apps
// @Produce      text/event-stream
// @Param        name path string true "Project name"
// @Success      200  {string}  string  "stream"
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      404  {object}  models.ErrorResponse  "not found"
// @Failure      500  {object}  models.ErrorResponse
// @Router       /api/projects/{name}/spark-apps/stream [get]
func (h *SparkHandler) StreamSparkApps(c *gin.Context) {
	events, err := h.service.WatchApps(c.Request.Context(), c.Param("name"))
	if err != nil {
		writeError(c, err)
		return
	}

	serveStream(c, events, func(s *sseStream, event models.SparkAppEvent) error {
		return s.Event("message", gin.H{"type": event.Type, "object": event.Object})
	})
}

// GetSparkApp godoc
// @Summary      Get a Spark application
// @Description  Get a specific SparkApplication instance
// @Tags         spark-apps
// @Produce      json
// @Param        name path string true "Project name"
// @Param        appName path string true "Spark application name"
// @Success      200  {object}  models.SparkAppInstance
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      404  {object}  models.ErrorResponse
// @Failure      500  {object}  models.ErrorResponse
// @Router       /api/projects/{name}/spark-apps/{appName} [get]
func (h *SparkHandler) GetSparkApp(c *gin.Context) {
	project := c.Param("name")
	appName := c.Param("appName")

	instance, err := h.service.GetApp(c.Request.Context(), project, appName)
	if err != nil {
		if errors.Is(err, apperr.ErrNotFound) {
			c.JSON(http.StatusNotFound, models.ErrorResponse{
				Error: fmt.Sprintf("SparkApplication '%s' not found in project '%s'", appName, project),
			})
			return
		}
		writeError(c, err, "Failed to get Spark application")
		return
	}
	c.JSON(http.StatusOK, instance)
}

// DeleteSparkApp godoc
// @Summary      Delete a Spark application
// @Description  Delete a SparkApplication from a project
// @Tags         spark-apps
// @Produce      json
// @Param        name path string true "Project name"
// @Param        appName path string true "Spark application name"
// @Success      204
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      404  {object}  models.ErrorResponse
// @Failure      500  {object}  models.ErrorResponse
// @Router       /api/projects/{name}/spark-apps/{appName} [delete]
func (h *SparkHandler) DeleteSparkApp(c *gin.Context) {
	project := c.Param("name")
	appName := c.Param("appName")

	if err := h.service.DeleteApp(c.Request.Context(), project, appName); err != nil {
		if errors.Is(err, apperr.ErrNotFound) {
			c.JSON(http.StatusNotFound, models.ErrorResponse{
				Error: fmt.Sprintf("SparkApplication '%s' not found in project '%s'", appName, project),
			})
			return
		}
		writeError(c, err, "Failed to delete Spark application")
		return
	}
	c.Status(http.StatusNoContent)
}

// UpdateSparkApp godoc
// @Summary      Update a Spark application
// @Description  Update an existing SparkApplication spec fields, checked as on submit (image, sparkConf); its driver and executors run as the project's spark-rbac ServiceAccount, and the fields the server does not build are dropped.
// @Tags         spark-apps
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name"
// @Param        appName path string true "Spark application name"
// @Param        request body models.SparkAppUpdateRequest true "Spark Application Update"
// @Success      200  {object}  models.SparkAppInstance
// @Failure      400  {object}  models.ErrorResponse
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      404  {object}  models.ErrorResponse
// @Failure      413  {object}  models.ErrorResponse  "the request body is too large"
// @Failure      500  {object}  models.ErrorResponse
// @Router       /api/projects/{name}/spark-apps/{appName} [put]
func (h *SparkHandler) UpdateSparkApp(c *gin.Context) {
	project := c.Param("name")
	appName := c.Param("appName")

	var req models.SparkAppUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: err.Error()})
		return
	}

	instance, err := h.service.UpdateApp(c.Request.Context(), project, appName, req)
	if err != nil {
		if errors.Is(err, apperr.ErrNotFound) {
			c.JSON(http.StatusNotFound, models.ErrorResponse{
				Error: fmt.Sprintf("SparkApplication '%s' not found in project '%s'", appName, project),
			})
			return
		}
		if service.IsValidationError(err) {
			c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: err.Error()})
			return
		}
		writeError(c, err, "Failed to update Spark application")
		return
	}
	c.JSON(http.StatusOK, instance)
}

// GetSparkUI godoc
// @Summary      Get Spark application driver UI info
// @Description  Returns the Spark driver Web UI service info for a running SparkApplication
// @Tags         spark-apps
// @Produce      json
// @Param        name path string true "Project name"
// @Param        appName path string true "Spark application name"
// @Success      200  {object}  models.SparkUIInfo
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      404  {object}  models.ErrorResponse
// @Failure      500  {object}  models.ErrorResponse
// @Router       /api/projects/{name}/spark-apps/{appName}/ui [get]
func (h *SparkHandler) GetSparkUI(c *gin.Context) {
	project := c.Param("name")
	appName := c.Param("appName")

	info, err := h.service.GetSparkUI(c.Request.Context(), project, appName)
	if err != nil {
		if errors.Is(err, apperr.ErrNotFound) {
			c.JSON(http.StatusNotFound, models.ErrorResponse{
				Error: fmt.Sprintf("SparkApplication '%s' not found in project '%s'", appName, project),
			})
			return
		}
		writeError(c, err, "Failed to get Spark UI info")
		return
	}
	c.JSON(http.StatusOK, info)
}

// GetSparkAppLogs godoc
// @Summary      Get Spark application driver logs
// @Description  Retrieve logs for a Spark application's driver pod. Use follow=true for streaming via SSE.
// @Tags         spark-apps
// @Produce      text/plain
// @Param        name path string true "Project name"
// @Param        appName path string true "Spark application name"
// @Param        container query string false "Container name (defaults to spark-kubernetes-driver)"
// @Param        tailLines query int false "Number of lines from the end (default 100, at most 10000)"
// @Param        follow query bool false "Stream logs in real-time (default false)"
// @Success      200  {string}  string  "log output"
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      404  {object}  models.ErrorResponse
// @Failure      500  {object}  models.ErrorResponse
// @Router       /api/projects/{name}/spark-apps/{appName}/logs [get]
func (h *SparkHandler) GetSparkAppLogs(c *gin.Context) {
	project := c.Param("name")
	appName := c.Param("appName")
	container := c.Query("container")
	follow := c.Query("follow") == "true"

	tailLines := tailLinesParam(c)

	stream, err := h.service.GetDriverLogs(c.Request.Context(), project, appName, container, tailLines, follow)
	if err != nil {
		if errors.Is(err, apperr.ErrNotFound) {
			c.JSON(http.StatusNotFound, models.ErrorResponse{
				Error: fmt.Sprintf("SparkApplication '%s' not found in project '%s'", appName, project),
			})
			return
		}
		writeError(c, err, "Failed to get Spark application logs")
		return
	}
	defer stream.Close()

	writeLogs(c, stream, follow)
}

// GetSparkConfig godoc
// @Summary      Get Spark operator configuration
// @Description  Returns Spark operator configuration from the platform values, global.okdp.sparkOperator (images, defaults, versions)
// @Tags         spark-apps
// @Produce      json
// @Success      200  {object}  models.SparkConfig
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      500  {object}  models.ErrorResponse
// @Router       /api/spark-config [get]
func (h *SparkHandler) GetSparkConfig(c *gin.Context) {
	cfg, err := h.service.GetSparkConfig(c.Request.Context())
	if err != nil {
		writeError(c, err, "Failed to get Spark configuration")
		return
	}
	c.JSON(http.StatusOK, cfg)
}

// GetSparkAppSchema godoc
// @Summary      Get SparkApplication CRD schema
// @Description  Returns the OpenAPI spec schema from the SparkApplication CRD installed by the Spark Operator
// @Tags         spark-apps
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      401  {object}  models.ErrorResponse  "no valid token"
// @Failure      403  {object}  models.ErrorResponse  "code forbidden: the permission the route needs (permission, project)"
// @Failure      404  {object}  models.ErrorResponse
// @Failure      500  {object}  models.ErrorResponse
// @Router       /api/spark-app-schema [get]
func (h *SparkHandler) GetSparkAppSchema(c *gin.Context) {
	schema, err := h.service.GetAppSchema(c.Request.Context())
	if err != nil {
		slog.Warn("Failed to get SparkApplication CRD schema", "error", err)
		c.JSON(http.StatusNotFound, models.ErrorResponse{Error: err.Error()})
		return
	}
	c.JSON(http.StatusOK, schema)
}
