// Package app is the composition root: the one sanctioned place where
// concrete types are constructed and wired together.
//
// Two rules hold the process lifecycle together, and everything else follows
// from them:
//
//   - workers() is a single table listing every long-lived goroutine, in drain
//     order. No package outside this one starts a goroutine that outlives a
//     request; services expose a blocking Run(ctx) and this table starts it.
//   - Shutdown walks that table in order, so "stop accepting" always happens
//     before "finish what you accepted", which always happens before the
//     maintenance loops go away.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/spf13/pflag"

	"github.com/arasHi87/gogolook-task-api/internal/api"
	"github.com/arasHi87/gogolook-task-api/internal/buildinfo"
	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/logging"
	"github.com/arasHi87/gogolook-task-api/internal/task"
	"github.com/arasHi87/gogolook-task-api/internal/task/memrepo"
)

// Mode selects which halves of the system this process runs.
type Mode string

const (
	// ModeServe runs the HTTP API and consumes no jobs.
	ModeServe Mode = "serve"
	// ModeWorker consumes jobs and exposes no public listener (the admin
	// listener stays up, so it is still scrapeable and probeable).
	ModeWorker Mode = "worker"
	// ModeAll runs both in one process: the default for `go run` and for a
	// single-container demo.
	ModeAll Mode = "all"
)

// Runs reports whether this mode serves HTTP requests.
func (m Mode) Runs() bool { return m == ModeServe || m == ModeAll }

// Consumes reports whether this mode consumes jobs.
func (m Mode) Consumes() bool { return m == ModeWorker || m == ModeAll }

// App holds everything the process owns for its lifetime.
type App struct {
	mode Mode
	log  *logging.Handle

	// cfg is read by request-path code and written by SIGHUP, so it is behind
	// a mutex rather than passed around by value.
	mu  sync.RWMutex
	cfg *config.Config

	// configFile and flags are remembered so a SIGHUP re-runs the *same* merge
	// the process started with. Re-reading only the file would silently drop
	// every value that came from a flag — a process started with -v would go
	// quiet on the first reload.
	configFile string
	flags      *pflag.FlagSet
	// reloaded is signalled after each successful reload; tests wait on it.
	reloaded chan struct{}

	// Wired once in New so a misconfiguration is a startup error rather than a
	// surprise on the first request.
	tasks     *task.Service
	apiServer *http.Server

	closers []func() error
}

// Options are what New needs from the CLI layer.
type Options struct {
	Mode       Mode
	Config     *config.Config
	ConfigFile string
	// Flags is the parsed flag set the initial configuration was built from.
	// It is replayed on reload so precedence is identical every time.
	Flags  *pflag.FlagSet
	Logger *logging.Handle
}

// New wires the application. It does not start anything; Run does that.
//
// Everything that can fail to be built fails here, at startup, rather than on
// the first request that needs it.
func New(o Options) (*App, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}

	a := &App{
		mode:       o.Mode,
		log:        o.Logger,
		cfg:        o.Config,
		configFile: o.ConfigFile,
		flags:      o.Flags,
		reloaded:   make(chan struct{}, 1),
	}

	repo, err := newRepository(o.Config)
	if err != nil {
		return nil, err
	}
	a.tasks = task.NewService(repo)

	if o.Mode.Runs() {
		if a.apiServer, err = a.newAPIServer(o.Config); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// validate checks the wiring contract. These are programming errors, not
// operator errors, so they say what the caller got wrong.
func (o Options) validate() error {
	switch {
	case o.Config == nil:
		return errors.New("app: config is required")
	case o.Logger == nil:
		return errors.New("app: logger is required")
	}
	switch o.Mode {
	case ModeServe, ModeWorker, ModeAll:
		return nil
	default:
		return fmt.Errorf("app: unknown mode %q", o.Mode)
	}
}

// newAPIServer builds the public listener: the REST contract, the Connect
// surface, the documentation, and the middleware chain around them.
func (a *App) newAPIServer(cfg *config.Config) (*http.Server, error) {
	handler, err := api.NewMux(api.MuxOptions{
		Service:      a.tasks,
		MaxBodyBytes: cfg.HTTP.MaxBodyBytes,
	})
	if err != nil {
		return nil, err
	}
	return newHTTPServer(cfg.HTTP, handler, a.log.Logger, "api"), nil
}

// newRepository selects the storage backend.
//
// memory is the default and needs nothing: it is what makes `go run` work with
// no Postgres, no Docker and no configuration, which is the literal requirement
// the exercise states. postgres is what everything built on top of it needs.
func newRepository(cfg *config.Config) (task.Repository, error) {
	switch cfg.Storage.Backend {
	case config.BackendMemory:
		return memrepo.New(), nil
	case config.BackendPostgres:
		return nil, fmt.Errorf("storage backend %q is not wired yet", cfg.Storage.Backend)
	default:
		return nil, fmt.Errorf("unknown storage backend %q", cfg.Storage.Backend)
	}
}

// Tasks exposes the domain service, for tests and for the worker wiring.
func (a *App) Tasks() *task.Service { return a.tasks }

// Config returns the current effective configuration. Callers must treat the
// result as read-only; SIGHUP replaces the pointer rather than mutating it.
func (a *App) Config() *config.Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg
}

// Mode reports which halves of the system this process runs.
func (a *App) Mode() Mode { return a.mode }

// Logger returns the process logger handle.
func (a *App) Logger() *logging.Handle { return a.log }

// workers is the drain-order table. Read top to bottom, it is also the shutdown
// sequence: stop accepting new work, finish what was accepted, then let the
// maintenance loops go.
//
// Everything that runs for longer than one request appears here, and nothing
// appears anywhere else.
func (a *App) workers() []Worker {
	var ws []Worker

	// Drain order, top to bottom. The public listener goes first: stop
	// accepting new work before anything that might be needed to finish the
	// work already accepted is taken away.
	if a.apiServer != nil {
		ws = append(ws, serveWorker("api", a.apiServer, a.log.Logger))
	}

	// Last, because a config reload racing a shutdown helps nobody.
	ws = append(ws, Worker{Name: "config-reloader", Run: a.runReloader})

	return ws
}

// Run starts every worker and blocks until ctx is cancelled or one of them
// fails. It then drains in table order and returns the first error seen.
func (a *App) Run(ctx context.Context) error {
	workers := a.workers()
	cfg := a.Config()

	// version is already on every record via the logger's base attributes.
	info := buildinfo.Get()
	a.log.Info("starting",
		slog.String("mode", string(a.mode)),
		slog.String("commit", info.Commit),
		slog.String("go", info.GoVersion),
		slog.String("instance", cfg.Service.Instance),
		slog.String("storage", cfg.Storage.Backend),
		slog.String("config_file", orNone(a.configFile)),
		slog.Int("workers", len(workers)),
	)

	return runWorkers(ctx, a.log.Logger, workers, cfg.HTTP.ShutdownGrace.D())
}

// Close releases resources registered during wiring, in reverse order.
func (a *App) Close() error {
	var errs []error
	for i := len(a.closers) - 1; i >= 0; i-- {
		if err := a.closers[i](); err != nil {
			errs = append(errs, err)
		}
	}
	a.closers = nil
	return errors.Join(errs...)
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}
