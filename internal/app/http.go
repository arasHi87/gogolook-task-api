package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/arasHi87/gogolook-task-api/internal/admin"
	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/logging"
	"github.com/arasHi87/gogolook-task-api/internal/runtime"
)

// newHTTPServer builds a listener with every timeout set.
//
// Each of these exists because its absence is a documented way to lose a
// server: no ReadHeaderTimeout is a Slowloris, no ReadTimeout lets a slow body
// hold a goroutine indefinitely, no WriteTimeout lets a stalled client do the
// same on the way out, and no IdleTimeout leaves keep-alive connections parked
// forever. Go's defaults are all "no limit".
func newHTTPServer(cfg config.HTTP, handler http.Handler, log *slog.Logger, name string) *http.Server {
	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout.D(),
		ReadTimeout:       cfg.ReadTimeout.D(),
		WriteTimeout:      cfg.WriteTimeout.D(),
		IdleTimeout:       cfg.IdleTimeout.D(),
		// The base context carries the process logger, so every handler's
		// logging.From(ctx) works without a global.
		BaseContext: func(net.Listener) context.Context {
			return logging.Into(context.Background(), log)
		},
		// net/http's own error log is unstructured and goes to the standard
		// logger; route it into ours at WARN, where "client sent an invalid
		// request" belongs.
		ErrorLog: slog.NewLogLogger(log.With(slog.String("listener", name)).Handler(), slog.LevelWarn),
	}
}

// serveWorker adapts an http.Server to the worker table.
//
// Stop is Shutdown rather than a context cancellation, and that is the whole
// point of Worker having a Stop at all: cancelling the context would drop
// requests that are already in flight, while Shutdown stops accepting and lets
// them finish.
func serveWorker(name string, srv *http.Server, log *slog.Logger) Worker {
	return Worker{
		Name: name,
		Run: func(ctx context.Context) error {
			var lc net.ListenConfig
			ln, err := lc.Listen(ctx, "tcp", srv.Addr)
			if err != nil {
				return fmt.Errorf("listen on %s: %w", srv.Addr, err)
			}
			log.Info("listening",
				slog.String("listener", name),
				slog.String("addr", ln.Addr().String()),
			)

			// A server that has been asked to shut down returns this, and it is
			// the normal path, not a failure.
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
		Stop: func(ctx context.Context) error {
			if err := srv.Shutdown(ctx); err != nil {
				// The deadline ran out with requests still in flight. Close
				// what is left rather than leave the process hanging.
				_ = srv.Close()
				return fmt.Errorf("drain %s: %w", name, err)
			}
			return nil
		},
	}
}

// buildAPI constructs the public listener, in the modes that serve.
func (a *App) buildAPI(cfg *config.Config) error {
	if !a.mode.Runs() {
		return nil
	}

	srv, err := a.newAPIServer(cfg)
	if err != nil {
		return err
	}
	a.apiServer = srv
	return nil
}

// buildAdmin constructs the private listener.
//
// Last, and it has to be: its readiness probes the dependencies every step
// above it opened, and its debug surface renders the configuration they were
// built from.
//
// It runs in every mode, including worker. A process with no public port still
// has to be scrapeable and probeable.
func (a *App) buildAdmin(cfg *config.Config) error {
	a.health = admin.New(a.readinessChecks()...)
	a.adminServer = newHTTPServer(adminHTTP(cfg), a.health.Mux(a.debug()), a.log.Logger, "admin")
	return nil
}

// newAPIServer builds the public listener: the REST contract, the Connect
// surface, the documentation, and the middleware chain around them.
func (a *App) newAPIServer(cfg *config.Config) (*http.Server, error) {
	handler, err := runtime.NewHandler(runtime.Options{
		Service:           a.tasks,
		MaxBodyBytes:      cfg.HTTP.MaxBodyBytes,
		Idempotency:       a.keys,
		IdempotencyConfig: cfg.Idempotency,
		Auth:              a.auth,
		RateLimit:         a.limiter,
		GlobalInflight:    inflightLimit(cfg),
		Metrics:           a.metrics,
		Tracing:           cfg.Observability.Tracing.Enabled,
	})
	if err != nil {
		return nil, err
	}
	return newHTTPServer(cfg.HTTP, handler, a.log.Logger, "api"), nil
}

// adminHTTP borrows the public listener's timeouts for the private one. They
// are the same kind of server with the same failure modes, and a second set of
// knobs nobody tunes is a second set of knobs to get wrong.
func adminHTTP(cfg *config.Config) config.HTTP {
	h := cfg.HTTP
	h.Addr = cfg.Admin.Addr
	return h
}
