package handlers

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/authz"
)

// streamKeepalive is the period of a stream's comment line, which keeps
// proxies from cutting an idle stream, and of the check that its caller
// still holds the route's permission (streamGuard).
var streamKeepalive = 30 * time.Second

// streamGuard keeps a server-sent stream to what its token allows: the
// route's permission, checked again at each keepalive from the authority
// as it is now (a member removed from the project loses the stream within
// streamKeepalive plus authz.CacheTTL), and the token's own expiry, at
// which the stream ends: the console reconnects with a fresher token, and
// a 401 or 403 answers it then.
type streamGuard struct {
	ctx    context.Context
	timer  *time.Timer
	expiry <-chan time.Time
}

// newStreamGuard guards the stream of the request of c.
func newStreamGuard(c *gin.Context) *streamGuard {
	g := &streamGuard{ctx: c.Request.Context()}
	if id := auth.IdentityFromContext(g.ctx); id != nil && !id.ExpiresAt.IsZero() {
		g.timer = time.NewTimer(time.Until(id.ExpiresAt))
		g.expiry = g.timer.C
	}
	return g
}

// Stop releases the guard.
func (g *streamGuard) Stop() {
	if g.timer != nil {
		g.timer.Stop()
	}
}

// Expiry is told when the token's validity ends.
func (g *streamGuard) Expiry() <-chan time.Time { return g.expiry }

// Allowed reads whether the caller still holds the route's permission
// (authz.RecheckRoute); an unreadable authority ends the stream, the
// caller reconnects through the check of a new request.
func (g *streamGuard) Allowed() bool {
	ctx, err := authz.RecheckRoute(g.ctx)
	g.ctx = ctx
	if err != nil && !errors.Is(err, authz.ErrForbidden) {
		slog.Warn("Could not read the platform's authority: ending a stream", "error", err)
	}
	return err == nil
}

// Context is the request's context, its caller the last Allowed read.
func (g *streamGuard) Context() context.Context { return g.ctx }

// The reasons a stream gives when it ends (sseStream.end).
const (
	streamEndedExpired   = "the token expired"
	streamEndedForbidden = "the access to this stream was withdrawn"
)
