package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/okdp/okdp-control-plane-server/internal/api/handlers"
	"github.com/okdp/okdp-control-plane-server/internal/buildinfo"
	"github.com/okdp/okdp-control-plane-server/internal/config"
)

// shutdownTimeout bounds the requests in flight at a stop: a commit and
// its Secret, and the undoing of a half written pair (compensationTimeout).
// Below the chart's terminationGracePeriodSeconds.
const shutdownTimeout = 45 * time.Second

// @title           OKDP Server New API
// @version         1.0
// @description     Minimal API server for OKDP UI New. Every answer carries an X-Request-Id header; a 500 (and a 502 of the SQL routes) answers {"error": <generic message>, "requestId": <id>}, its cause in the server's log under that id. Every route needs a permission, deny by default, read from the platform's own authority: platform admin (a group of platform.yaml global.okdp.access.admins) for the identity routes, the catalog's writes and the creation and deletion of a project; a project role (project.yaml access, okdp-project-<p> an editor by default; a platform admin has every role) for the routes of /api/projects/{name}: viewer for its reads, editor for its writes, admin for its project.yaml. A caller without it answers 403 {"error": <what it needs>, "code": "forbidden"}; GET /api/me tells what the caller may do.
// @termsOfService  http://swagger.io/terms/

// @contact.name    API Support
// @contact.url     http://www.swagger.io/support
// @contact.email   support@swagger.io

// @license.name    Apache 2.0
// @license.url     http://www.apache.org/licenses/LICENSE-2.0.html

// @host            localhost:8093
// @BasePath        /

// @securityDefinitions.apikey BearerAuth
// @in                         header
// @name                       Authorization
// @description                OIDC access token, as "Bearer <token>". Required on
// @description                every /api route except /api/capabilities.

// @security                   BearerAuth
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// A second signal is not waited for: the default action, at once.
	context.AfterFunc(ctx, stop)
	if err := run(ctx); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

// run starts the server and serves until ctx ends, then stops it.
func run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}
	initLogger(cfg)
	for _, setting := range cfg.InsecureSettings() {
		slog.Warn(fmt.Sprintf("WARNING: insecure setting %s. Never outside a sandbox.", setting))
	}

	// Release mode unless asked otherwise: debug mode prints every route and
	// warns on each start, which is noise in a pod. GIN_MODE=debug brings it back.
	if os.Getenv(gin.EnvGinMode) == "" {
		gin.SetMode(gin.ReleaseMode)
	}

	d, err := newDeps(cfg)
	if err != nil {
		return err
	}
	routes, err := d.router(ctx)
	if err != nil {
		return err
	}

	// Not r.Run: it leaves every timeout at zero, so a client that opens a
	// connection and sends its headers one byte at a time holds a goroutine
	// indefinitely, and an idle keep-alive is never reclaimed.
	//
	// WriteTimeout stays unset on purpose. Three routes stream Server-Sent
	// Events (projects, services, spark apps) with a deadline of their own
	// on each write; one for the whole response would cut them mid-flight.
	server := &http.Server{
		Addr:              ":" + cfg.ServerPort,
		Handler:           routes,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// The streams end at the signal; the requests' own contexts do not,
		// for a commit and its Secret to be written to the end.
		BaseContext: func(net.Listener) context.Context {
			return handlers.WithDraining(context.Background(), ctx.Done())
		},
	}

	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("failed to start server: %w", err)
	}
	slog.Info("Starting server", "port", cfg.ServerPort, "version", buildinfo.CurrentVersion())
	return serve(ctx, server, listener, shutdownTimeout)
}

// serve serves listener until ctx ends, then waits for the requests in
// flight, within timeout (SRV-26).
func serve(ctx context.Context, server *http.Server, listener net.Listener, timeout time.Duration) error {
	failed := make(chan error, 1)
	go func() { failed <- server.Serve(listener) }()

	select {
	case err := <-failed:
		return fmt.Errorf("failed to start server: %w", err)
	case <-ctx.Done():
	}

	slog.Info("Stopping: waiting for the requests in flight")
	stopCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := server.Shutdown(stopCtx); err != nil {
		_ = server.Close()
		return fmt.Errorf("requests still in flight after %s: %w", timeout, err)
	}
	if err := <-failed; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// initLogger sets the level and the format of the logs: time, level and
// message, the level words as they have always been (debug, info, warning,
// error), then the fields by name.
func initLogger(cfg *config.Config) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel(cfg.LogLevel),
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) > 0 {
				return a
			}
			switch a.Key {
			case slog.TimeKey:
				if t, ok := a.Value.Any().(time.Time); ok {
					a.Value = slog.StringValue(t.Format(time.RFC3339))
				}
			case slog.LevelKey:
				if level, ok := a.Value.Any().(slog.Level); ok {
					a.Value = slog.StringValue(levelWord(level))
				}
			}
			return a
		},
	})))
}

// logLevel reads LOG_LEVEL, info for what it does not name.
func logLevel(name string) slog.Level {
	switch strings.ToLower(name) {
	case "trace":
		return slog.LevelDebug - 4
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "fatal", "panic":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// levelWord names a level in the log's own vocabulary.
func levelWord(level slog.Level) string {
	switch {
	case level < slog.LevelDebug:
		return "trace"
	case level < slog.LevelInfo:
		return "debug"
	case level < slog.LevelWarn:
		return "info"
	case level < slog.LevelError:
		return "warning"
	}
	return "error"
}
