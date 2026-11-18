package middleware

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
)

// RequestLogger logs incoming HTTP requests.
func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path

		c.Next()

		latency := time.Since(start)
		statusCode := c.Writer.Status()

		attrs := []any{
			"status", statusCode,
			"method", c.Request.Method,
			"path", path,
			"ip", c.ClientIP(),
			"latency", latency.String(), // Text output is cleaner than ns
			"duration", latency,
		}
		if id := RequestIDOf(c); id != "" {
			attrs = append(attrs, "requestId", id)
		}
		if actor := CallerIdentity(c); actor != nil {
			attrs = append(attrs, "actorSubject", actor.Subject, "actorUsername", actor.Username)
		}

		switch {
		case len(c.Errors) > 0:
			slog.Error(c.Errors.String(), attrs...)
		case statusCode >= 500:
			slog.Error("Internal Server Error", attrs...)
		case statusCode >= 400:
			slog.Warn("Bad Request", attrs...)
		default:
			slog.Info("Request Handled", attrs...)
		}
	}
}
