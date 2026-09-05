package queue_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/outbox"
	"github.com/arasHi87/gogolook-task-api/internal/queue"
	"github.com/arasHi87/gogolook-task-api/internal/testenv"
)

// testConfig is fast enough that a lease test finishes in seconds rather than
// half a minute, while keeping the relationships the real defaults have.
func testConfig() config.Queue {
	c := config.Defaults().Queue
	c.Workers = 4
	c.ClaimBatch = 5
	c.Lease = config.Duration(900 * time.Millisecond)
	c.HeartbeatInterval = config.Duration(300 * time.Millisecond)
	c.PollInterval = config.Duration(100 * time.Millisecond)
	c.ReaperInterval = config.Duration(150 * time.Millisecond)
	c.FetchCooldown = 0
	c.JobTimeout = config.Duration(5 * time.Second)
	c.MaxAttempts = 3
	c.Backoff.Base = config.Duration(10 * time.Millisecond)
	c.Backoff.Max = config.Duration(50 * time.Millisecond)
	return c
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// enqueue writes n jobs of one kind, each in its own transaction.
func enqueue(t *testing.T, pool *pgxpool.Pool, kind string, n int) {
	t.Helper()

	for i := range n {
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := outbox.Enqueue(t.Context(), tx, outbox.Job{
			Kind: kind, Payload: map[string]int{"n": i},
		}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
}

// runPool starts a pool with the maintenance loops beside it.
//
// Both, because the pool alone cannot retry anything: a failed job goes to
// retryable, and only the scheduler moves it back to available. That
// separation is deliberate — it is what stops a hot-failing job from being
// instantly re-claimable and monopolising the claim query — but it means a
// fleet with no maintenance running never retries. TestRetriesNeedTheScheduler
// pins that down.
//
// Nothing here sleeps waiting for work. Tests wait on the event channel with a
// deadline, which is what stops a queue suite becoming the flaky one everybody
// skips.
func runPool(t *testing.T, pool *pgxpool.Pool, cfg config.Queue, handlers map[string]queue.HandlerFunc) (*queue.Pool, <-chan queue.Event) {
	t.Helper()
	return runQueue(t, pool, cfg, handlers, true)
}

// runPoolOnly starts the workers with no maintenance, for tests that need to
// observe what the pool does on its own.
func runPoolOnly(t *testing.T, pool *pgxpool.Pool, cfg config.Queue, handlers map[string]queue.HandlerFunc) (*queue.Pool, <-chan queue.Event) {
	t.Helper()
	return runQueue(t, pool, cfg, handlers, false)
}

func runQueue(
	t *testing.T,
	pool *pgxpool.Pool,
	cfg config.Queue,
	handlers map[string]queue.HandlerFunc,
	maintain bool,
) (*queue.Pool, <-chan queue.Event) {
	t.Helper()

	store := queue.NewStore(pool)
	p, err := queue.NewPool(queue.PoolOptions{
		Store:    store,
		Handlers: handlers,
		WorkerID: "worker-" + t.Name(),
		Config:   cfg,
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	events, unsubscribe := p.Subscribe(1024)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = p.Run(ctx)
	}()

	if maintain {
		m := queue.NewMaintenance(pool, store, cfg, testLogger(), nil)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.Run(ctx)
		}()
	}

	t.Cleanup(func() {
		cancel()
		wg.Wait()
		unsubscribe()
	})
	return p, events
}

// await waits for n events, or fails with what it did see.
func await(t *testing.T, events <-chan queue.Event, n int, within time.Duration) []queue.Event {
	t.Helper()

	got := make([]queue.Event, 0, n)
	deadline := time.After(within)
	for len(got) < n {
		select {
		case e := <-events:
			got = append(got, e)
		case <-deadline:
			t.Fatalf("timed out after %s with %d of %d events: %+v", within, len(got), n, got)
		}
	}
	return got
}

// awaitOutcome waits for one event with a given outcome, ignoring others.
func awaitOutcome(t *testing.T, events <-chan queue.Event, want queue.Outcome, within time.Duration) queue.Event {
	t.Helper()

	deadline := time.After(within)
	var seen []queue.Event
	for {
		select {
		case e := <-events:
			if e.Outcome == want {
				return e
			}
			seen = append(seen, e)
		case <-deadline:
			t.Fatalf("timed out after %s waiting for %s; saw %+v", within, want, seen)
		}
	}
}

// jobRow reads one job's bookkeeping.
type jobRow struct {
	State       queue.State
	Attempt     int
	LockedBy    *string
	Errors      []map[string]any
	AttemptedBy []string
}

func readJob(t *testing.T, pool *pgxpool.Pool, id int64) jobRow {
	t.Helper()

	var (
		r    jobRow
		errs []byte
	)
	const q = `SELECT state, attempt, locked_by, errors, attempted_by FROM jobs WHERE id = $1`
	if err := pool.QueryRow(t.Context(), q, id).Scan(&r.State, &r.Attempt, &r.LockedBy, &errs, &r.AttemptedBy); err != nil {
		t.Fatalf("read job %d: %v", id, err)
	}
	if err := json.Unmarshal(errs, &r.Errors); err != nil {
		t.Fatalf("decode errors of job %d: %v", id, err)
	}
	return r
}

func onlyJobID(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()

	var id int64
	if err := pool.QueryRow(t.Context(), `SELECT id FROM jobs`).Scan(&id); err != nil {
		t.Fatalf("read job id: %v", err)
	}
	return id
}

func newPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return testenv.Postgres(t)
}
