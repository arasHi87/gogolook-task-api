package harness

import (
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arasHi87/gogolook-task-api/internal/testenv"
)

// DemoToken is the plaintext behind the compiled-in hash, published in
// .env.example so the quickstart and these scenarios agree on one credential.
const DemoToken = "demo-standard-token"

// System is one live Task API, with the verbs to drive it.
type System struct {
	t *testing.T

	// API drives the canonical /api/v1 surface.
	API *API
	// Webhook is the receiver the worker delivers to.
	Webhook *Webhook
	// Jobs and Tasks are the rows the processes left behind.
	Jobs  *Jobs
	Tasks *Rows
	// Keys is the idempotency table.
	Keys *Rows
	// Admin and Public are the two listeners, so a scenario can assert that
	// something answers on one and not the other.
	Admin  *Probe
	Public *Probe
	// Worker is the consumer process, to crash or to drain.
	Worker *Process
	// Server is the api process.
	Server *Process
	// DB is a pool on the same database the processes use.
	DB *pgxpool.Pool

	dsn       string
	workerEnv []string
}

// Option tunes a system for one scenario.
type Option func(*options)

type options struct {
	noWorker  bool
	workerEnv []string
	apiEnv    []string
}

// WithoutWorker starts the api alone, for scenarios about the write path that
// should not race a consumer draining the queue underneath them.
func WithoutWorker() Option { return func(o *options) { o.noWorker = true } }

// WorkerEnv overrides worker configuration, e.g. a shorter lease.
func WorkerEnv(kv ...string) Option {
	return func(o *options) { o.workerEnv = append(o.workerEnv, kv...) }
}

// APIEnv overrides api configuration, e.g. a small rate-limit quota.
func APIEnv(kv ...string) Option {
	return func(o *options) { o.apiEnv = append(o.apiEnv, kv...) }
}

// Start brings a system up and returns once it is serving.
func Start(t *testing.T, opts ...Option) *System {
	t.Helper()

	var o options
	for _, apply := range opts {
		apply(&o)
	}

	// A private, already-migrated database per scenario. Scenarios can then
	// run in parallel without a TRUNCATE between them deleting each other's
	// jobs — which produces failures that read as application bugs.
	dsn := testenv.DSN(t)
	pool := testenv.PostgresAt(t, dsn)

	sys := &System{
		t:       t,
		dsn:     dsn,
		DB:      pool,
		Webhook: newWebhook(t),
		Jobs:    &Jobs{t: t, pool: pool},
		Tasks:   &Rows{t: t, pool: pool, table: "tasks"},
		Keys:    &Rows{t: t, pool: pool, table: "idempotency_keys"},
	}

	sys.Server = startProcess(t, "api", []string{"serve"}, append(sys.baseEnv("api"), o.apiEnv...))
	sys.API = newAPI(t, "http://"+sys.Server.addr("api")+"/api/v1")
	sys.Public = &Probe{t: t, base: "http://" + sys.Server.addr("api")}
	sys.Admin = &Probe{t: t, base: "http://" + sys.Server.addr("admin")}
	sys.awaitReady(sys.Server)

	if !o.noWorker {
		sys.workerEnv = append(sys.baseEnv("worker"), sys.queueEnv()...)
		sys.workerEnv = append(sys.workerEnv, o.workerEnv...)
		sys.StartWorker()
	}
	return sys
}

// StartWorker brings a consumer up and waits for it to be ready.
func (s *System) StartWorker() {
	s.t.Helper()

	s.Worker = startProcess(s.t, "worker", []string{"worker"}, s.workerEnv)
	s.awaitReady(s.Worker)
}

// CrashWorker kills the consumer outright: no drain, no chance to release a
// lease, nothing written on the way out. It is the only honest way to test the
// reaper.
func (s *System) CrashWorker() {
	s.t.Helper()
	s.Worker.kill()
}

// DrainWorker stops the consumer the way an orchestrator does, and requires it
// to exit cleanly.
func (s *System) DrainWorker() {
	s.t.Helper()

	s.Worker.signal(syscall.SIGTERM)
	// Released here rather than by the caller: a drain that is waiting on a
	// held delivery would otherwise hit the grace period and be reported as a
	// slow drain, which is the opposite of what the scenario is asserting.
	s.Webhook.Release()

	if err := s.Worker.stop(30 * time.Second); err != nil {
		s.t.Errorf("worker drain: %v", err)
	}
}

// WorkerLog blocks until the consumer logs a message, which is how a scenario
// waits for a mechanism — the reaper reclaiming, the circuit opening — rather
// than for the clock.
func (s *System) WorkerLog(msg string) Record {
	s.t.Helper()

	return s.Worker.Await(func(r Record) bool {
		return strings.Contains(r.str("msg"), msg)
	}, awaitTimeout, msg)
}

// WorkerMetrics is the consumer's own /metrics, for the gauges only it
// publishes.
func (s *System) WorkerMetrics() string {
	s.t.Helper()
	return (&Probe{t: s.t, base: "http://" + s.Worker.addr("admin")}).Get("/metrics").Status(200).Body
}

// baseEnv is the configuration both processes share.
//
// Everything arrives through TASKAPI_* rather than flags because that is how
// the container is configured, and because it exercises the environment layer
// of the four-layer merge on every single run.
func (s *System) baseEnv(instance string) []string {
	return []string{
		"TASKAPI_SERVICE_INSTANCE=" + instance,
		"TASKAPI_LOGGING_FORMAT=json",
		"TASKAPI_LOGGING_LEVEL=debug",
		"TASKAPI_STORAGE_BACKEND=postgres",
		"TASKAPI_STORAGE_POSTGRES_DSN=" + s.dsn,
		// Port 0 on both listeners: the process picks, logs what it got, and
		// the harness reads it back. Nothing here competes for a fixed port.
		"TASKAPI_HTTP_ADDR=127.0.0.1:0",
		"TASKAPI_ADMIN_ADDR=127.0.0.1:0",
		// The anonymous quota is raised out of the way. Every scenario is
		// anonymous unless it says otherwise, and a suite that tripped the
		// default 20-request burst would report the limiter working as a
		// product bug in whatever scenario happened to be tenth.
		"TASKAPI_RATELIMIT_TIERS_ANONYMOUS_RATE=10000",
		"TASKAPI_RATELIMIT_TIERS_ANONYMOUS_BURST=10000",
	}
}

// queueEnv compresses the queue's timings so a lease can expire inside a
// scenario.
//
// The relationships the defaults have are preserved — heartbeat is a third of
// the lease, the reaper runs several times per lease — because a suite that
// changes the relationships is testing a different system.
func (s *System) queueEnv() []string {
	return []string{
		"TASKAPI_WEBHOOK_URL=" + s.Webhook.URL(),
		"TASKAPI_WEBHOOK_TIMEOUT=5s",
		"TASKAPI_QUEUE_WORKERS=4",
		"TASKAPI_QUEUE_LEASE=3s",
		"TASKAPI_QUEUE_HEARTBEAT_INTERVAL=1s",
		"TASKAPI_QUEUE_POLL_INTERVAL=250ms",
		"TASKAPI_QUEUE_REAPER_INTERVAL=500ms",
		"TASKAPI_QUEUE_FETCH_COOLDOWN=0s",
		"TASKAPI_QUEUE_JOB_TIMEOUT=20s",
		"TASKAPI_QUEUE_MAX_ATTEMPTS=5",
		"TASKAPI_QUEUE_BACKOFF_BASE=200ms",
		"TASKAPI_QUEUE_BACKOFF_MAX=1s",
	}
}

// awaitReady polls the process's own readiness endpoint.
//
// Its own, not a socket dial: a listening socket says the port is bound, while
// /readyz says the database probe passed. Starting a scenario against a process
// that cannot reach Postgres produces a failure about tasks.
func (s *System) awaitReady(p *Process) {
	s.t.Helper()

	url := "http://" + p.addr("admin") + "/readyz"
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(30 * time.Second)

	for {
		resp, err := client.Get(url) //nolint:noctx // bounded by the client timeout
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			p.dump()
			s.t.Fatalf("%s: not ready within 30s (last error: %v)", p.name, err)
		}
		if !p.running() {
			p.dump()
			s.t.Fatalf("%s: exited before becoming ready", p.name)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Probe reads one listener, so a scenario can assert what answers where.
type Probe struct {
	t    *testing.T
	base string
}

// Get fetches a path.
func (p *Probe) Get(path string) *Response { return p.do(http.MethodGet, path, "") }

// Put sends a plain-text body, which is what the log-level knob takes.
func (p *Probe) Put(path, body string) *Response { return p.do(http.MethodPut, path, body) }

func (p *Probe) do(method, path, body string) *Response {
	p.t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(p.t.Context(), method, p.base+path, reader)
	if err != nil {
		p.t.Fatalf("%s %s: %v", method, path, err)
	}

	resp, err := (&http.Client{Timeout: requestTimeout}).Do(req)
	if err != nil {
		p.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // fully read below

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		p.t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return &Response{t: p.t, what: method + " " + p.base + path, resp: resp, Body: string(raw)}
}
