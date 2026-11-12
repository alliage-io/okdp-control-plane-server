package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"

	"github.com/gin-gonic/gin"
)

// RequestIDHeader carries the id of a request, in and out.
const RequestIDHeader = "X-Request-Id"

// requestIDKey is where RequestID puts the id in the gin context.
const requestIDKey = "requestId"

// requestIDPattern is what a client-sent id may be: anything else is
// replaced, a log line never carries what a client chose.
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)

// RequestID names every request: the X-Request-Id a client (or a proxy)
// sent when it is a plain token, else a random one. It is answered in the
// X-Request-Id header, logged with the request, and put in the body of an
// internal error, so an answer can be matched with the server's log.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(RequestIDHeader)
		if !requestIDPattern.MatchString(id) {
			var b [12]byte
			_, _ = rand.Read(b[:])
			id = hex.EncodeToString(b[:])
		}
		c.Set(requestIDKey, id)
		c.Header(RequestIDHeader, id)
		c.Next()
	}
}

// RequestIDOf returns the id RequestID gave the request, "" without it.
func RequestIDOf(c *gin.Context) string {
	return c.GetString(requestIDKey)
}
