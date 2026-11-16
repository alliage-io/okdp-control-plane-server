package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"github.com/okdp/okdp-control-plane-server/internal/authz"
	"github.com/okdp/okdp-control-plane-server/internal/models"
)

// ErrorCodeForbidden is the code of a 403: the caller lacks the right
// the route needs.
const ErrorCodeForbidden = "forbidden"

// Permissions is the router's table: the permission each route needs,
// by RouteKey.
type Permissions map[string]authz.Permission

// RouteKey is a route of the table: its method and its full path, as gin
// registers it ("GET /api/projects/:name").
func RouteKey(method, path string) string { return method + " " + path }

// Authorize refuses, deny by default, a request whose caller lacks the
// permission the table gives its route: 403, {"error", "code":
// "forbidden", "permission", "project"} (ForbiddenBody). A route missing
// from the table is refused whoever calls it. A project permission is of the project of the route's :name; a
// platform admin holds every project role. The caller, kept in the
// request's context (authz.CallerFrom), is the verified token's groups
// read against the authority; with authentication off (a nil authority,
// AUTH_DISABLED) every caller holds every right.
func Authorize(table Permissions, authority *authz.Authority, authenticationOff bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		perm, ok := table[RouteKey(c.Request.Method, c.FullPath())]
		if !ok {
			forbidden(c, "this route is not open to anyone")
			return
		}
		var caller *authz.Caller
		switch identity := CallerIdentity(c); {
		case authenticationOff:
			caller = authz.Unrestricted()
		case identity != nil:
			// A nil authority knows no admin and no project role.
			caller = authority.Caller(identity.Groups)
		}
		c.Request = c.Request.WithContext(authz.WithCaller(c.Request.Context(), caller))
		project := c.Param("name")
		allowed, err := caller.Allows(c.Request.Context(), perm, project)
		if err != nil {
			id := RequestIDOf(c)
			logrus.WithError(err).WithField("requestId", id).WithField("path", c.Request.URL.Path).Error("Could not read the platform's authority")
			c.AbortWithStatusJSON(http.StatusInternalServerError, models.ErrorResponse{Error: "Request failed", RequestID: id})
			return
		}
		if !allowed {
			refused := authz.Forbidden(perm, project)
			c.AbortWithStatusJSON(http.StatusForbidden, ForbiddenBody(refused.Error(), refused))
			return
		}
		c.Next()
	}
}

// ForbiddenBody is the body of a 403 code forbidden: message, and the
// permission refused (permission, its project for a project role) when
// there is one.
func ForbiddenBody(message string, refused *authz.ForbiddenError) models.ErrorResponse {
	body := models.ErrorResponse{Error: message, Code: ErrorCodeForbidden}
	if refused != nil {
		body.Permission = refused.Permission.Name()
		body.Project = refused.Project
	}
	return body
}

func forbidden(c *gin.Context, message string) {
	c.AbortWithStatusJSON(http.StatusForbidden, ForbiddenBody(message, nil))
}
