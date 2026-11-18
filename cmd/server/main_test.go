package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/okdp/okdp-control-plane-server/internal/config"
)

// A request in flight at the signal is served to its end: the commit and
// the Secret of a write are not cut (SRV-26).
func TestServeLetsARequestInFlightFinish(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	handlerCtxErr := make(chan error, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		handlerCtxErr <- r.Context().Err()
		_, _ = io.WriteString(w, "done")
	})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	served := make(chan error, 1)
	go func() { served <- serve(ctx, server, listener, 10*time.Second) }()

	body := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + listener.Addr().String())
		if err != nil {
			body <- err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		body <- string(b)
	}()
	<-entered
	stop()
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-served:
		t.Fatalf("serve returned with a request in flight: %v", err)
	default:
	}
	close(release)

	if got := <-body; got != "done" {
		t.Errorf("the client read %q", got)
	}
	if err := <-handlerCtxErr; err != nil {
		t.Errorf("the handler's context ended at the signal: %v", err)
	}
	if err := <-served; err != nil {
		t.Errorf("serve: %v", err)
	}
}

// A request past the bound does not hold the stop forever.
func TestServeGivesUpAfterTheTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	served := make(chan error, 1)
	go func() { served <- serve(ctx, server, listener, 20*time.Millisecond) }()
	go func() {
		if resp, err := http.Get("http://" + listener.Addr().String()); err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	stop()
	if err := <-served; err == nil {
		t.Error("no error for a request still in flight")
	}
}

// The logs keep their level words and field names.
func TestTheLogFormat(t *testing.T) {
	var out bytes.Buffer
	cfg := &config.Config{LogLevel: "warning"}
	original := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func(previous *slog.Logger) { slog.SetDefault(previous); os.Stderr = original }(slog.Default())
	initLogger(cfg)
	slog.Info("hidden")
	slog.Warn("shown", "actorUsername", "alice", "error", errors.New("boom"))
	slog.Error("failed")
	_ = w.Close()
	if _, err := io.Copy(&out, r); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "hidden") {
		t.Errorf("an info line at level warning: %s", got)
	}
	for _, want := range []string{"level=warning", "msg=shown", "actorUsername=alice", "error=boom", "level=error"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in %s", want, got)
		}
	}
}

func TestLogLevels(t *testing.T) {
	for name, want := range map[string]slog.Level{
		"": slog.LevelInfo, "bogus": slog.LevelInfo, "info": slog.LevelInfo, "INFO": slog.LevelInfo,
		"debug": slog.LevelDebug, "warn": slog.LevelWarn, "warning": slog.LevelWarn, "error": slog.LevelError,
		"fatal": slog.LevelError, "panic": slog.LevelError,
	} {
		if got := logLevel(name); got != want {
			t.Errorf("%q: %v, want %v", name, got, want)
		}
	}
}
