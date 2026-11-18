package authz

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// A name no project can have is answered without the source, the cache or
// the log: a URL's :name, unbounded in number and size (SRV-20).
func TestAnInvalidProjectNameCostsNothing(t *testing.T) {
	var reads atomic.Int64
	source := countingSource{reads: &reads, fakeSource: fakeSource{}}
	a := NewAuthority(source)
	recorded := &recordingHandler{}
	defer slog.SetDefault(slog.Default())
	slog.SetDefault(slog.New(recorded))

	for i := range 2000 {
		name := strings.Repeat("x", 1<<10) + "_" + string(rune('a'+i%26))
		access, err := a.ProjectAccess(context.Background(), name)
		if err != nil || len(access) != 0 {
			t.Fatalf("%q: %v %v", name, access, err)
		}
	}
	if a.projects.len() != 0 || reads.Load() != 0 {
		t.Errorf("kept %d entries, %d reads", a.projects.len(), reads.Load())
	}
	for _, message := range recorded.messages() {
		t.Errorf("logged: %s", message)
	}
}

// The access kept is bounded, the least recently read dropped first.
func TestTheProjectCacheIsBounded(t *testing.T) {
	var reads atomic.Int64
	a := NewAuthority(countingSource{reads: &reads, fakeSource: fakeSource{}})
	a.projects = newAccessCache(3)
	ctx := context.Background()

	for _, p := range []string{"a", "b", "c", "a", "d"} {
		if _, err := a.ProjectAccess(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if got := a.projects.len(); got != 3 {
		t.Errorf("%d entries", got)
	}
	if a.projects.get("b") != nil {
		t.Error("b, the least recently read, is still kept")
	}
	for _, p := range []string{"a", "c", "d"} {
		if a.projects.get(p) == nil {
			t.Errorf("%s dropped", p)
		}
	}
	reads.Store(0)
	if _, err := a.ProjectAccess(ctx, "b"); err != nil || reads.Load() != 1 {
		t.Errorf("reads %d, %v", reads.Load(), err)
	}
}

type countingSource struct {
	fakeSource
	reads *atomic.Int64
}

func (c countingSource) ProjectFile(ctx context.Context, p string) ([]byte, error) {
	c.reads.Add(1)
	return c.fakeSource.ProjectFile(ctx, p)
}

// A stream's periodic check reads the authority as it is now: the role
// withdrawn, the memoized platform-admin answer dropped (SRV-21).
func TestRecheckRouteReadsTheAuthorityAnew(t *testing.T) {
	source := &mutableSource{fakeSource{
		platform: "global:\n  okdp:\n    access:\n      admins: [platform_admin]\n",
		projects: map[string]string{"demo": "access:\n  analysts: viewer\n"},
	}}
	a := NewAuthority(source)
	a.ttl = 0
	ctx := context.Background()

	caller := a.Caller([]string{"analysts"})
	route := WithRoute(WithCaller(ctx, caller), ProjectViewer, "demo")
	if _, err := RecheckRoute(route); err != nil {
		t.Fatalf("a member: %v", err)
	}
	source.projects = map[string]string{"demo": "access:\n  others: viewer\n"}
	if _, err := RecheckRoute(route); !errors.Is(err, ErrForbidden) {
		t.Errorf("the group withdrawn: %v", err)
	}

	admin := a.Caller([]string{"platform_admin"})
	if is, _ := admin.IsPlatformAdmin(ctx); !is {
		t.Fatal("not an admin")
	}
	routeAdmin := WithRoute(WithCaller(ctx, admin), PlatformAdmin, "")
	source.platform = "global:\n  okdp:\n    access:\n      admins: [someone_else]\n"
	if _, err := RecheckRoute(routeAdmin); !errors.Is(err, ErrForbidden) {
		t.Errorf("admin withdrawn, memoized: %v", err)
	}
	// A context without a route is not re-checked.
	if _, err := RecheckRoute(ctx); err != nil {
		t.Errorf("no route: %v", err)
	}
}

type mutableSource struct{ fakeSource }

func (m *mutableSource) ProjectFile(ctx context.Context, p string) ([]byte, error) {
	return m.fakeSource.ProjectFile(ctx, p)
}
func (m *mutableSource) PlatformValues(ctx context.Context) ([]byte, error) {
	return m.fakeSource.PlatformValues(ctx)
}

// recordingHandler keeps the messages logged.
type recordingHandler struct {
	mu   sync.Mutex
	logs []string
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler            { return h }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.logs = append(h.logs, r.Message)
	return nil
}

func (h *recordingHandler) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.logs...)
}
