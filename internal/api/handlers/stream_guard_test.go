package handlers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/authz"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

// streamSource: an authority's tree whose project.yaml a test changes.
type streamSource struct {
	mu      sync.Mutex
	project string
}

func (s *streamSource) set(project string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.project = project
}

func (s *streamSource) PlatformValues(context.Context) ([]byte, error) { return nil, nil }

func (s *streamSource) ProjectFile(context.Context, string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []byte(s.project), nil
}

type watchSpy struct {
	service.ServiceService
	events chan service.ServiceEvent
	logs   func() io.ReadCloser
}

func (s watchSpy) WatchServices(context.Context, string) (<-chan service.ServiceEvent, error) {
	return s.events, nil
}

func (s watchSpy) GetPodLogs(context.Context, string, string, string, string, int64, bool) (io.ReadCloser, error) {
	return s.logs(), nil
}

// streamRequest serves the route of register with a caller of the group
// analysts, a viewer of project demo through source, its token valid until
// expires (never for the zero time), and returns the body the client read
// until the stream ended.
func streamRequest(t *testing.T, source *streamSource, expires time.Time, register func(*gin.Engine), target string) string {
	t.Helper()
	authority := authz.NewAuthority(source)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		ctx := auth.WithIdentity(c.Request.Context(), &auth.Identity{Username: "u", ExpiresAt: expires})
		caller := authority.Caller([]string{"analysts"})
		ctx = authz.WithRoute(authz.WithCaller(ctx, caller), authz.ProjectViewer, c.Param("name"))
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	register(engine)
	server := httptest.NewServer(engine)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("the stream did not end: %v (read %q)", err, body)
	}
	if ctx.Err() != nil {
		t.Fatalf("the stream did not end: %q", body)
	}
	return string(body)
}

func serviceStream(h *ServiceHandler) func(*gin.Engine) {
	return func(e *gin.Engine) { e.GET("/api/projects/:name/services/stream", h.StreamServices) }
}

func podLogs(h *ServiceHandler) func(*gin.Engine) {
	return func(e *gin.Engine) { e.GET("/api/projects/:name/services/:serviceName/pods/:pod/logs", h.GetPodLogs) }
}

// A stream outlives its request's authorization by at most a keepalive: a
// group removed from the project's access ends it, the watch and the log
// tail alike (SRV-21).
func TestAStreamEndsWhenTheCallerIsRemoved(t *testing.T) {
	old := streamKeepalive
	streamKeepalive = 10 * time.Millisecond
	defer func() { streamKeepalive = old }()

	for name, tc := range map[string]struct {
		register func(*ServiceHandler) func(*gin.Engine)
		target   string
	}{
		"services": {serviceStream, "/api/projects/demo/services/stream"},
		"pod logs": {podLogs, "/api/projects/demo/services/s/pods/p/logs?follow=true"},
	} {
		t.Run(name, func(t *testing.T) {
			source := &streamSource{project: "access:\n  analysts: viewer\n"}
			h := NewServiceHandler(watchSpy{
				events: make(chan service.ServiceEvent),
				logs:   func() io.ReadCloser { return blockingLog{} },
			}, nil)
			time.AfterFunc(50*time.Millisecond, func() { source.set("access:\n  others: viewer\n") })
			body := streamRequest(t, source, time.Time{}, tc.register(h), tc.target)
			if !strings.Contains(body, streamEndedForbidden) {
				t.Errorf("no end event in %q", body)
			}
		})
	}
}

// A stream ends when its token does, without waiting for the keepalive.
func TestAStreamEndsAtTheTokensExpiry(t *testing.T) {
	old := streamKeepalive
	streamKeepalive = time.Hour
	defer func() { streamKeepalive = old }()

	source := &streamSource{project: "access:\n  analysts: viewer\n"}
	h := NewServiceHandler(watchSpy{events: make(chan service.ServiceEvent)}, nil)
	start := time.Now()
	body := streamRequest(t, source, time.Now().Add(80*time.Millisecond), serviceStream(h), "/api/projects/demo/services/stream")
	if !strings.Contains(body, streamEndedExpired) {
		t.Errorf("no end event in %q", body)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("ended after %s", elapsed)
	}
}

type blockingLog struct{}

func (blockingLog) Read(p []byte) (int, error) {
	time.Sleep(time.Hour)
	return 0, fmt.Errorf("unreachable")
}
func (blockingLog) Close() error { return nil }
