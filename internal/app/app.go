// Package app is the composition root: the one sanctioned place where
// concrete types are constructed and wired together.
//
// Two tables describe the whole process, and everything else is the detail
// behind one of them:
//
//   - New's build table is the dependency order. Read top to bottom it says
//     what this process is made of, and each entry is a no-op in the modes
//     that do not need it.
//   - workers() is the drain order. It lists every goroutine that outlives a
//     request, and Shutdown walks it in order, so "stop accepting" always
//     happens before "finish what you accepted", which always happens before
//     the maintenance loops go away.
//
// No package outside this one starts a goroutine that outlives a request;
// services expose a blocking Run(ctx) and the worker table starts it.
//
// One file per concern, so that a change to how storage is opened is a change
// to storage.go and not a diff in the middle of six hundred lines:
//
//	app.go            the App, the build table, the lifecycle
//	observability.go  metrics, tracing, the collectors, the debug surface
//	storage.go        the repository, the pool, the readiness probes
//	guards.go         auth, the rate limiter, idempotency
//	queue.go          the consumer side
//	http.go           the two listeners
//	workers.go        the drain-order table
//	runner.go         starting and draining that table
//	reload.go         SIGHUP
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/pflag"

	"github.com/arasHi87/gogolook-task-api/internal/admin"
	"github.com/arasHi87/gogolook-task-api/internal/auth"
	"github.com/arasHi87/gogolook-task-api/internal/buildinfo"
	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/idempotency"
	"github.com/arasHi87/gogolook-task-api/internal/logging"
	"github.com/arasHi87/gogolook-task-api/internal/metrics"
	"github.com/arasHi87/gogolook-task-api/internal/queue"
	"github.com/arasHi87/gogolook-task-api/internal/ratelimit"
	"github.com/arasHi87/gogolook-task-api/internal/task"
	"github.com/arasHi87/gogolook-task-api/internal/tracing"
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
	tasks       *task.Service
	apiServer   *http.Server
	adminServer *http.Server
	health      *admin.Handler
	pool        *pgxpool.Pool

	// keys remembers what an Idempotency-Key produced. Both backends have one:
	// the memory store makes `go run` honour the header rather than silently
	// dropping it.
	keys      idempotency.Store
	keysPurge *idempotency.Purger

	// The inbound guards. auth resolves the caller, limiter meters them.
	auth    *auth.Resolver
	limiter *ratelimit.Limiter

	// metrics is built before anything that reports into it.
	metrics *metrics.Registry
	// tracing is built first of all: the pool, the queue and the middleware
	// chain all take a tracer from it.
	tracing *tracing.Provider

	// The queue runs only on the postgres backend: it is the outbox, and an
	// in-memory store has no transaction to write one in.
	jobs        *queue.Pool
	maintenance *queue.Maintenance
	listener    *queue.Listener

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
// The build order below is a dependency order, and reading it top to bottom is
// the shortest honest description of what this process is made of. It is a
// table for the same reason workers() is: the order is a decision, and a
// decision spread across a hundred lines of straight-line code is a decision
// nobody can review.
//
// Every step is a no-op in the modes that do not need it, so the table is the
// same in all three and the differences live where they belong — next to the
// thing that differs.
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

	build := []struct {
		what string
		fn   func(*config.Config) error
	}{
		{"observability", a.buildObservability},
		{"storage", a.buildStorage},
		{"guards", a.buildGuards},
		{"api", a.buildAPI},
		{"queue", a.buildQueue},
		{"collectors", a.registerCollectors},
		{"admin", a.buildAdmin},
	}
	for _, step := range build {
		if err := step.fn(o.Config); err != nil {
			return nil, fmt.Errorf("app: build %s: %w", step.what, err)
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
