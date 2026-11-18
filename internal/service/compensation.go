package service

import (
	"context"
	"time"
)

// compensationTimeout bounds the undoing of the first half of a pair of
// writes (a commit and its Secret, a namespace and its commit).
const compensationTimeout = 30 * time.Second

// compensationContext is the context the undoing of a write runs with:
// the request's values (the actor), not its cancellation. A client gone
// after a commit must not leave the commit without what it names, the
// timeout bounding what the client no longer waits for.
func compensationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), compensationTimeout)
}
