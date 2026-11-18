package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// deadlineRecorder records the deadlines the stream sets on the response.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
	err       error
}

func (r *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	r.deadlines = append(r.deadlines, t)
	return r.err
}

func streamContext(t *testing.T, w http.ResponseWriter, ctx context.Context) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/stream", nil).WithContext(ctx)
	return c
}

// Each write has its own deadline, and none is left on the connection the
// server keeps for its next request.
func TestAStreamSetsAWriteDeadlineAndClearsIt(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	c := streamContext(t, rec, context.Background())
	events := make(chan string, 1)
	events <- "hello"
	close(events)

	serveStream(c, events, func(s *sseStream, e string) error { return s.Event("message", e) })

	if len(rec.deadlines) < 3 {
		t.Fatalf("%d deadlines: the headers, the event and the clearing expected", len(rec.deadlines))
	}
	for _, d := range rec.deadlines[:len(rec.deadlines)-1] {
		if d.IsZero() || time.Until(d) > streamWriteTimeout {
			t.Errorf("deadline %v", d)
		}
	}
	if last := rec.deadlines[len(rec.deadlines)-1]; !last.IsZero() {
		t.Errorf("the deadline is left on the connection: %v", last)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event:message\ndata:hello\n\n") {
		t.Errorf("body %q", body)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type %q", got)
	}
	for _, header := range []string{"Transfer-Encoding", "Connection"} {
		if got := rec.Header().Get(header); got != "" {
			t.Errorf("%s set by hand: %q", header, got)
		}
	}
}

// A connection refusing the deadline ends the stream at once.
func TestAStreamEndsWhenTheDeadlineCannotBeSet(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder(), err: errors.New("connection gone")}
	c := streamContext(t, rec, context.Background())
	events := make(chan string)
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveStream(c, events, func(*sseStream, string) error { return nil })
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end")
	}
}

// The signal ends a stream, which would otherwise hold the stop until its
// timeout; the client is told nothing and reconnects.
func TestAStreamEndsWhenTheServerStops(t *testing.T) {
	draining := make(chan struct{})
	rec := httptest.NewRecorder()
	c := streamContext(t, rec, WithDraining(context.Background(), draining))
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveStream(c, make(chan string), func(*sseStream, string) error { return nil })
	}()
	close(draining)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end")
	}
	if strings.Contains(rec.Body.String(), "event:error") {
		t.Errorf("an error event at a stop: %q", rec.Body.String())
	}
}

// A send that fails ends the stream.
func TestAStreamEndsWhenSendFails(t *testing.T) {
	c := streamContext(t, httptest.NewRecorder(), context.Background())
	events := make(chan string, 2)
	events <- "a"
	events <- "b"
	sent := 0
	serveStream(c, events, func(*sseStream, string) error {
		sent++
		return errors.New("write failed")
	})
	if sent != 1 {
		t.Errorf("%d sends after a failure", sent)
	}
}
