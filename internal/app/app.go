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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/pflag"

	"github.com/arasHi87/gogolook-task-api/internal/admin"
	"github.com/arasHi87/gogolook-task-api/internal/buildinfo"
	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/logging"
	"github.com/arasHi87/gogolook-task-api/internal/postgres"
	"github.com/arasHi87/gogolook-task-api/internal/runtime"
	"github.com/arasHi87/gogolook-task-api/internal/task"
	"github.com/arasHi87/gogolook-task-api/internal/task/memrepo"
	"github.com/arasHi87/gogolook-task-api/internal/task/pgrepo"
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

	repo, err := a.newRepository(o.Config)
	if err != nil {
		return nil, err
	}
	a.tasks = task.NewService(repo)

	if o.Mode.Runs() {
		if a.apiServer, err = a.newAPIServer(o.Config); err != nil {
			return nil, err
		}
	}

	// The admin listener runs in every mode, including worker: a process with
	// no public port still has to be scrapeable and probeable.
	a.health = admin.New(a.readinessChecks()...)
	a.adminServer = newHTTPServer(adminHTTP(o.Config), a.health.Mux(), o.Logger.Logger, "admin")

	return a, nil
}

// readinessChecks are the dependencies this process needs to serve.
//
// There are none on the memory backend, which is correct rather than lazy: it
// has no dependency that can be down.
func (a *App) readinessChecks() []admin.Check {
	if a.pool == nil {
		return nil
	}
	return []admin.Check{{
		Name: "postgres",
		// Ping, not a query: readiness asks whether a connection can be had,
		// and a SELECT would also be reporting on the query planner.
		Probe: a.pool.Ping,
	}}
}

// adminHTTP borrows the public listener's timeouts for the private one. They
// are the same kind of server with the same failure modes, and a second set of
// knobs nobody tunes is a second set of knobs to get wrong.
func adminHTTP(cfg *config.Config) config.HTTP {
	h := cfg.HTTP
	h.Addr = cfg.Admin.Addr
	return h
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
	handler, err := runtime.NewHandler(runtime.Options{
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
// no Postgres, no Docker and no configuration, which is the literal
// requirement the exercise states. postgres adds durability, the transactional
// outbox and the queue.
func (a *App) newRepository(cfg *config.Config) (task.Repository, error) {
	switch cfg.Storage.Backend {
	case config.BackendMemory:
		return memrepo.New(), nil

	case config.BackendPostgres:
		// The pool is opened here rather than lazily, so an unreachable
		// database is a startup failure with a clear message instead of a
		// confusing error on the first request.
		role := postgres.RoleAPI
		if a.mode == ModeWorker {
			role = postgres.RoleWorker
		}
		pool, err := postgres.Open(context.Background(), cfg.Storage.Postgres, role, cfg.Service.Instance)
		if err != nil {
			return nil, err
		}
		a.pool = pool
		a.closers = append(a.closers, func() error { pool.Close(); return nil })

		return pgrepo.New(pool), nil

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

	// Drain order, top to bottom, and each position is a decision:
	//
	//   readiness       first, so a load balancer stops routing here before
	//                   the listener stops accepting. The requests that arrive
	//                   in that window are still served.
	//   api             stop accepting, finish what was accepted.
	//   config-reloader a reload racing a shutdown helps nobody.
	//   admin           last, so health and metrics answer for the whole
	//                   drain rather than going dark at the start of it.
	ws = append(ws, Worker{
		Name: "readiness",
		Run:  waitForShutdown,
		Stop: func(context.Context) error {
			a.health.StartDraining()
			a.log.Info("readiness flipped to not-ready, draining")
			return nil
		},
	})

	if a.apiServer != nil {
		ws = append(ws, serveWorker("api", a.apiServer, a.log.Logger))
	}

	ws = append(ws, Worker{Name: "config-reloader", Run: a.runReloader})
	ws = append(ws, serveWorker("admin", a.adminServer, a.log.Logger))

	return ws
}

// waitForShutdown is the Run of a worker whose whole job is its Stop.
func waitForShutdown(ctx context.Context) error {
	<-ctx.Done()
	return nil
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
