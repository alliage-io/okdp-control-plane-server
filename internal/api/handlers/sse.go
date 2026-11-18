package handlers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-contrib/sse"
	"github.com/gin-gonic/gin"
)

// streamWriteTimeout bounds one write of a stream: a client that stops
// reading ends its stream, it does not hold a goroutine and a watch. The
// server has no WriteTimeout, which would cut every stream mid-flight.
var streamWriteTimeout = 15 * time.Second

type drainingKey struct{}

// WithDraining gives the streams of the requests of ctx what tells them the
// server is stopping: they end at once, the console reconnects to the next
// server, and http.Server.Shutdown waits for the others, a commit and its
// Secret written to the end.
func WithDraining(ctx context.Context, draining <-chan struct{}) context.Context {
	return context.WithValue(ctx, drainingKey{}, draining)
}

// drainingOf is the channel of WithDraining, nil (never told) without one.
func drainingOf(ctx context.Context) <-chan struct{} {
	draining, _ := ctx.Value(drainingKey{}).(<-chan struct{})
	return draining
}

// sseStream is the response of a Server-Sent Events route: each write under
// its own deadline and flushed, the caller's right read again at each
// keepalive (streamGuard).
type sseStream struct {
	c          *gin.Context
	controller *http.ResponseController
	guard      *streamGuard
}

// write runs fn under a fresh write deadline and flushes what it wrote.
func (s *sseStream) write(fn func() error) error {
	if err := s.controller.SetWriteDeadline(time.Now().Add(streamWriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	return s.controller.Flush()
}

// Event sends one event; data that is not a string is sent as JSON.
func (s *sseStream) Event(name string, data any) error {
	return s.write(func() error {
		return sse.Encode(s.c.Writer, sse.Event{Event: name, Data: data})
	})
}

// comment sends a comment line, which keeps proxies from cutting an idle
// stream.
func (s *sseStream) comment(text string) error {
	return s.write(func() error {
		_, err := s.c.Writer.WriteString(": " + text + "\n\n")
		return err
	})
}

// end tells the client why the stream ends.
func (s *sseStream) end(reason string) {
	_ = s.Event("error", reason)
}

// Context is the request's context, its caller the last one the keepalive
// read.
func (s *sseStream) Context() context.Context { return s.guard.Context() }

// serveStream answers the request of c with a Server-Sent Events stream of
// events, each sent by send, until the channel closes, send fails, the
// client leaves, the token expires, the caller loses the route's permission
// or the server stops.
func serveStream[T any](c *gin.Context, events <-chan T, send func(*sseStream, T) error) {
	header := c.Writer.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")

	s := &sseStream{c: c, controller: http.NewResponseController(c.Writer), guard: newStreamGuard(c)}
	defer s.guard.Stop()
	// The server keeps the connection: a deadline left behind would cut the
	// next request served on it.
	defer func() { _ = s.controller.SetWriteDeadline(time.Time{}) }()

	if err := s.write(func() error { return nil }); err != nil {
		return
	}

	keepalive := time.NewTicker(streamKeepalive)
	defer keepalive.Stop()
	ctx := c.Request.Context()
	draining := drainingOf(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-draining:
			return
		case <-s.guard.Expiry():
			s.end(streamEndedExpired)
			return
		case <-keepalive.C:
			if !s.guard.Allowed() {
				s.end(streamEndedForbidden)
				return
			}
			if s.comment("keepalive") != nil {
				return
			}
		case event, ok := <-events:
			if !ok || send(s, event) != nil {
				return
			}
		}
	}
}
