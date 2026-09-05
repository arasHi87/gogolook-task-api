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
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/pflag"

	"github.com/arasHi87/gogolook-task-api/internal/admin"
	"github.com/arasHi87/gogolook-task-api/internal/auth"
	"github.com/arasHi87/gogolook-task-api/internal/buildinfo"
	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/idempotency"
	"github.com/arasHi87/gogolook-task-api/internal/logging"
	"github.com/arasHi87/gogolook-task-api/internal/metrics"
	"github.com/arasHi87/gogolook-task-api/internal/postgres"
	"github.com/arasHi87/gogolook-task-api/internal/queue"
	"github.com/arasHi87/gogolook-task-api/internal/queue/handler"
	"github.com/arasHi87/gogolook-task-api/internal/ratelimit"
	"github.com/arasHi87/gogolook-task-api/internal/resilience"
	"github.com/arasHi87/gogolook-task-api/internal/runtime"
	"github.com/arasHi87/gogolook-task-api/internal/task"
	"github.com/arasHi87/gogolook-task-api/internal/task/memrepo"
	"github.com/arasHi87/gogolook-task-api/internal/task/pgrepo"
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

	// First, because everything below reports into them.
	a.metrics = metrics.New(o.Config.Observability.Metrics)

	traces, err := tracing.New(
		o.Config.Observability.Tracing,
		o.Config.Service.Name, o.Config.Service.Instance, o.Logger.Logger,
	)
	if err != nil {
		return nil, err
	}
	a.tracing = traces

	repo, err := a.newRepository(o.Config)
	if err != nil {
		return nil, err
	}
	a.tasks = task.NewService(repo)
	a.newIdempotency(o.Config, o.Logger.Logger)
	a.newGuards(o.Config, o.Logger.Logger)

	if o.Mode.Runs() {
		if a.apiServer, err = a.newAPIServer(o.Config); err != nil {
			return nil, err
		}
	}

	if o.Mode.Consumes() && a.pool != nil {
		if err := a.newQueue(o.Config, o.Logger.Logger); err != nil {
			return nil, err
		}
	}

	if err := a.registerCollectors(o.Logger.Logger); err != nil {
		return nil, err
	}

	// The admin listener runs in every mode, including worker: a process with
	// no public port still has to be scrapeable and probeable.
	a.health = admin.New(a.readinessChecks()...)
	a.adminServer = newHTTPServer(adminHTTP(o.Config), a.health.Mux(a.debug()), o.Logger.Logger, "admin")

	return a, nil
}

// newIdempotency selects the key store, matching the storage backend.
//
// It has to match: a Postgres-backed service with an in-memory key store would
// honour a retry only when it landed on the same replica, which is worse than
// not honouring it at all — the failure is invisible and depends on the load
// balancer.
func (a *App) newIdempotency(cfg *config.Config, log *slog.Logger) {
	if !cfg.Idempotency.Enabled {
		return
	}

	if a.pool != nil {
		a.keys = idempotency.NewPostgresStore(a.pool)
	} else {
		a.keys = idempotency.NewMemoryStore()
	}
	a.keysPurge = idempotency.NewPurger(a.keys, cfg.Idempotency, log)
}

// newGuards wires the inbound protection: who the caller is, and how much of
// the service they may have.
//
// The resolver is built in every mode, including auth.mode=off, because the
// limiter keys on the identity it produces. Skipping it there would leave every
// anonymous caller sharing one bucket, which is worse than no limiter at all.
func (a *App) newGuards(cfg *config.Config, log *slog.Logger) {
	a.auth = auth.NewResolver(cfg.Auth, cfg.HTTP.TrustedProxyHops)

	if cfg.RateLimit.Enabled {
		a.limiter = ratelimit.New(cfg.RateLimit, log, a.metrics.Guards)
	}
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

// newQueue wires the consumer side: the listener that wakes it, the pool that
// claims and runs, and the maintenance loops one replica runs for everyone.
func (a *App) newQueue(cfg *config.Config, log *slog.Logger) error {
	store := queue.NewStore(a.pool)

	a.listener = queue.NewListener(cfg.Storage.Postgres.DSN, log)
	a.maintenance = queue.NewMaintenance(a.pool, store, cfg.Queue, log, a.metrics.Queue)

	// The webhook is the only genuinely remote thing this service talks to,
	// which makes it the only honest place for a circuit breaker.
	webhook := handler.NewWebhook(handler.Options{
		URL:      cfg.Webhook.URL,
		Timeout:  cfg.Webhook.Timeout.D(),
		Observer: a.metrics.Deps,
		Trace:    cfg.Observability.Tracing.Enabled,
		Guard: resilience.New(resilience.Options{
			Name:      "webhook",
			Breaker:   cfg.Breaker.Webhook,
			Timeout:   cfg.Webhook.Timeout.D(),
			Retry:     cfg.Webhook.Retry,
			Logger:    log,
			IsFailure: handler.IsDependencyFailure,
			Observer:  a.metrics.Guards,
		}),
	})

	jobs, err := queue.NewPool(queue.PoolOptions{
		Store: store,
		Handlers: map[string]queue.HandlerFunc{
			pgrepo.JobKind: webhook.Handle,
		},
		// locked_by has to be unique per process, or the stale-worker guard
		// cannot tell two replicas apart and a zombie can overwrite a live
		// worker's result.
		WorkerID: workerID(cfg.Service.Instance),
		Config:   cfg.Queue,
		Logger:   log,
		Notify:   a.listener.Notifications(),
		Observer: a.metrics.Queue,
		Tracer:   a.tracing.Tracer(),
	})
	if err != nil {
		return err
	}
	a.jobs = jobs
	a.metrics.Queue.Configured(cfg.Queue.Workers)
	return nil
}

// workerID identifies this process in the queue.
//
// The instance alone is not enough: two processes on one host, or a container
// restarted under the same name, would share an id and each could then finalize
// the other's jobs.
func workerID(instance string) string {
	return fmt.Sprintf("%s/%d/%s", instance, os.Getpid(), uuid.NewString()[:8])
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

// recordJobOutcomes turns the pool's completion events into metrics.
//
// A subscriber rather than instrumentation inside the pool, because the event
// already exists and already carries everything needed: the outcome, the wait
// from enqueue to claim, and the time in the handler. Adding a second
// reporting path inside the pool would be two places to keep in step.
//
// Delivery is non-blocking on the publisher's side, so a slow drain here drops
// events rather than stalling a worker. That is the right trade for metrics
// and the wrong one for the queue, which is why it is the queue's choice.
func (a *App) recordJobOutcomes(ctx context.Context) error {
	events, unsubscribe := a.jobs.Subscribe(256)
	defer unsubscribe()

	for {
		select {
		case <-ctx.Done():
			return nil
		case e, ok := <-events:
			if !ok {
				return nil
			}
			a.metrics.Queue.Finished(e)
		}
	}
}

// debug assembles the operator surface for the admin listener.
//
// Everything here is on the private port and nowhere else. A heap profile is a
// memory dump and the config dump names every dependency, so which port they
// are on is the security boundary rather than a convention.
func (a *App) debug() admin.Debug {
	d := admin.Debug{
		Level: a.log,
		Pprof: a.Config().Admin.Pprof,
		// The same renderer --print-config uses, so the two cannot disagree
		// about what is masked.
		Config: func(w io.Writer) error { return a.Config().WriteYAML(w) },
	}
	if a.metrics.Enabled() {
		d.Metrics = a.metrics.Handler()
	}
	return d
}

// registerCollectors adds the metrics that have to query something on scrape.
//
// They are registered here rather than inside internal/metrics because they
// need a pool and a store, and a metrics package that reached for a database
// would be instrumenting by owning.
func (a *App) registerCollectors(log *slog.Logger) error {
	if a.pool == nil {
		return nil
	}

	role := "api"
	if a.mode == ModeWorker {
		role = "worker"
	}
	if err := a.metrics.Register(metrics.NewPoolCollector(a.pool, role)); err != nil {
		return fmt.Errorf("register pool collector: %w", err)
	}

	// Only where the queue runs. A process that does not consume would report
	// a backlog it has nothing to do with, and two replicas reporting the same
	// global gauge is a sum that double counts.
	if a.mode.Consumes() {
		if err := a.metrics.Register(metrics.NewQueueCollector(queue.NewStore(a.pool), log)); err != nil {
			return fmt.Errorf("register queue collector: %w", err)
		}
	}
	return nil
}

// inflightLimit is the load-shedding cap, or zero when the limiter is off.
func inflightLimit(cfg *config.Config) int {
	if !cfg.RateLimit.Enabled {
		return 0
	}
	return cfg.RateLimit.GlobalInflight
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
		pool, err := postgres.OpenTraced(context.Background(), cfg.Storage.Postgres, role,
			cfg.Service.Instance, cfg.Observability.Tracing.Enabled)
		if err != nil {
			return nil, err
		}
		a.pool = pool
		a.closers = append(a.closers, func() error { pool.Close(); return nil })

		return pgrepo.New(pool, a.metrics.Queue), nil

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
	//   queue-metrics   drains the pool's completion events. After the pool,
	//                   so the last job's outcome is still recorded.
	//   idempotency-purge
	//   ratelimit-sweeper
	//                   housekeeping loops; nothing waits on them.
	//   config-reloader a reload racing a shutdown helps nobody.
	//   admin           near last, so health and metrics answer for the whole
	//                   drain rather than going dark at the start of it.
	//   tracing         last of all, flushing the spans every worker above it
	//                   produced on its way out.
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

	// The queue drains after the API: a job enqueued by the last request
	// accepted should still be picked up, and Stop hands back anything that
	// does not finish in time rather than letting its lease expire.
	if a.jobs != nil {
		ws = append(ws,
			Worker{Name: "queue-listener", Run: a.listener.Run},
			Worker{Name: "queue-workers", Run: a.jobs.Run, Stop: a.jobs.Stop},
			Worker{Name: "queue-maintenance", Run: a.maintenance.Run},
		)
	}

	// Leader-elected, so running it in every mode costs nothing and means the
	// keys keep being purged even when only workers are up.
	if a.keysPurge != nil {
		ws = append(ws, Worker{Name: "idempotency-purge", Run: a.keysPurge.Run})
	}

	// The queue's own metrics come from its completion events, which is the
	// only place the wait and the processing time are both known.
	if a.jobs != nil {
		ws = append(ws, Worker{Name: "queue-metrics", Run: a.recordJobOutcomes})
	}

	// The limiter's sweeper evicts idle buckets. It runs wherever the limiter
	// does, which is wherever there is a public listener.
	if a.limiter != nil && a.apiServer != nil {
		ws = append(ws, Worker{Name: "ratelimit-sweeper", Run: a.limiter.Run})
	}

	ws = append(ws, Worker{Name: "config-reloader", Run: a.runReloader})
	ws = append(ws, serveWorker("admin", a.adminServer, a.log.Logger))

	// Last, and it has to be: the exporter batches for up to five seconds, so
	// a process that exits without flushing loses the spans from whatever
	// caused the restart — which is the one trace anybody wanted.
	ws = append(ws, Worker{
		Name: "tracing",
		Run:  waitForShutdown,
		Stop: a.tracing.Shutdown,
	})

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
