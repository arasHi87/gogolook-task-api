package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arasHi87/gogolook-task-api/internal/testenv"
)

// harness is one live system: a private database, an api process, a worker
// process, and a webhook receiver, wired together and torn down with the test.
//
// The api and the worker are separate processes because that is how the stack
// deploys, and because it is the only way to make the claim honest. Two halves
// in one process share a heap, and a test that passes because of that has
// proved nothing about a job crossing a process boundary.
type harness struct {
	t *testing.T

	// API drives the canonical /api/v1 surface.
	API *client
	// Sink records the webhooks the worker delivers.
	Sink *sink
	// DB is a pool on the same database the processes use, for asserting on
	// the rows they wrote.
	DB *pgxpool.Pool

	dsn       string
	api       *process
	worker    *process
	workerEnv []string
}

// options tune a harness for one test.
type options struct {
	// noWorker starts the api alone, for tests about the write path that
	// should not race a consumer draining the queue underneath them.
	noWorker bool
	// workerEnv and apiEnv are appended last, so a test can override any
	// setting the harness chose.
	workerEnv []string
	apiEnv    []string
}

type option func(*options)

// withoutWorker leaves the queue unconsumed.
func withoutWorker() option { return func(o *options) { o.noWorker = true } }

// withWorkerEnv overrides worker configuration, e.g. a shorter lease.
func withWorkerEnv(kv ...string) option {
	return func(o *options) { o.workerEnv = append(o.workerEnv, kv...) }
}

// withAPIEnv overrides api configuration, e.g. a small rate-limit quota.
func withAPIEnv(kv ...string) option {
	return func(o *options) { o.apiEnv = append(o.apiEnv, kv...) }
}

// start brings a system up and returns once it is serving.
func start(t *testing.T, opts ...option) *harness {
	t.Helper()

	var o options
	for _, apply := range opts {
		apply(&o)
	}

	// A private, already-migrated database per test. Tests can then run in
	// parallel without a TRUNCATE between them deleting each other's jobs.
	dsn := testenv.DSN(t)

	h := &harness{
		t:    t,
		dsn:  dsn,
		Sink: newSink(t),
		DB:   testenv.PostgresAt(t, dsn),
	}

	h.api = startProcess(t, "api", []string{"serve"}, append(h.baseEnv("api"), o.apiEnv...))
	h.API = newClient(t, "http://"+h.api.addr("api")+"/api/v1")
	h.awaitReady(h.api)

	if !o.noWorker {
		h.workerEnv = append(h.baseEnv("worker"), h.queueEnv()...)
		h.workerEnv = append(h.workerEnv, o.workerEnv...)
		h.startWorker()
	}
	return h
}

// at returns a client for one path prefix on the same api process, so the
// contract can be driven against /api/v1 and the unversioned surface the
// assignment specifies without starting a second system.
func (h *harness) at(prefix string) *client {
	h.t.Helper()
	return newClient(h.t, "http://"+h.api.addr("api")+prefix)
}

// baseEnv is the configuration both processes share.
//
// Everything arrives through TASKAPI_* rather than flags because that is how
// the container is configured, and because it exercises the environment layer
// of the four-layer merge on every single run.
func (h *harness) baseEnv(instance string) []string {
	return []string{
		"TASKAPI_SERVICE_INSTANCE=" + instance,
		"TASKAPI_LOGGING_FORMAT=json",
		"TASKAPI_LOGGING_LEVEL=debug",
		"TASKAPI_STORAGE_BACKEND=postgres",
		"TASKAPI_STORAGE_POSTGRES_DSN=" + h.dsn,
		// Port 0 on both listeners: the process picks, logs what it got, and
		// the harness reads it back. Nothing here competes for a fixed port.
		"TASKAPI_HTTP_ADDR=127.0.0.1:0",
		"TASKAPI_ADMIN_ADDR=127.0.0.1:0",
		// The anonymous quota is raised out of the way. Every test here is
		// anonymous, and a suite that trips the default 20-request burst would
		// report the limiter working as a product bug in whatever test happened
		// to be tenth. TestTheRateLimiterShedsAnonymousTraffic lowers it back.
		"TASKAPI_RATELIMIT_TIERS_ANONYMOUS_RATE=10000",
		"TASKAPI_RATELIMIT_TIERS_ANONYMOUS_BURST=10000",
	}
}

// queueEnv compresses the queue's timings so a lease can expire inside a test.
//
// The relationships the defaults have are preserved — heartbeat is a third of
// the lease, the reaper runs several times per lease — because a test that
// changes the relationships is testing a different system.
func (h *harness) queueEnv() []string {
	return []string{
		"TASKAPI_WEBHOOK_URL=" + h.Sink.url(),
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

// startWorker brings a consumer up and waits for it to be ready.
func (h *harness) startWorker() {
	h.t.Helper()

	h.worker = startProcess(h.t, "worker", []string{"worker"}, h.workerEnv)
	h.awaitReady(h.worker)
}

// restartWorker replaces the consumer, as an orchestrator would after a crash.
// The new process gets a fresh worker id, which is the whole point: the stale
// worker guard has to be able to tell them apart.
func (h *harness) restartWorker() {
	h.t.Helper()
	h.startWorker()
}

// awaitReady polls the process's own readiness endpoint.
//
// Its own, not a socket dial: a listening socket says the port is bound, while
// /readyz says the database probe passed. Starting a test against a process
// that cannot reach Postgres produces a failure about tasks.
func (h *harness) awaitReady(p *process) {
	h.t.Helper()

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
			h.t.Fatalf("%s: not ready within 30s (last error: %v)", p.name, err)
		}
		if !p.running() {
			p.dump()
			h.t.Fatalf("%s: exited before becoming ready", p.name)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
