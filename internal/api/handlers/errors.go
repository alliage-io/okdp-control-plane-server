package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/okdp/okdp-control-plane-server/internal/api/middleware"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

// internalErrorMessage is what an internal error answers: its cause, a
// Kubernetes, Git or Keycloak answer, stays in the server's log.
const internalErrorMessage = "internal error: the server could not complete the request; quote the request id to its administrator"

// writeError answers err with the status of what it is: a missing feature
// (501), a missing resource (404), one that exists or changed meanwhile
// (409), a rejected input (400, with its code). Anything else is the
// server's own failure: 500 with a generic message and the request id, the
// cause logged with that id (logMessage, when given, says what failed).
func writeError(c *gin.Context, err error, logMessage ...string) {
	switch {
	case errors.Is(err, service.ErrConnectionsUnavailable):
		c.JSON(http.StatusNotImplemented, gin.H{"error": err.Error()})
	case apierrors.IsNotFound(err):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case apierrors.IsAlreadyExists(err), apierrors.IsConflict(err):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case service.IsValidationError(err):
		c.JSON(http.StatusBadRequest, validationBody(err))
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case isConflict(err):
		c.JSON(http.StatusConflict, gin.H{"error": "it exists already", "requestId": middleware.RequestIDOf(c)})
	default:
		writeInternal(c, http.StatusInternalServerError, err, logMessage...)
	}
}

// isConflict reports an error of an upstream API (Keycloak) refusing what
// exists already.
func isConflict(err error) bool {
	var conflict interface{ Conflict() bool }
	return errors.As(err, &conflict) && conflict.Conflict()
}

// writeInternal answers a failure of the server, or of what it called, with
// status, a generic message and the request id, and logs its cause.
func writeInternal(c *gin.Context, status int, err error, logMessage ...string) {
	id := middleware.RequestIDOf(c)
	message := "Request failed"
	if len(logMessage) > 0 {
		message = logMessage[0]
	}
	logrus.WithError(err).WithFields(logrus.Fields{"requestId": id, "path": c.Request.URL.Path}).Error(message)
	c.JSON(status, gin.H{"error": internalErrorMessage, "requestId": id})
}
