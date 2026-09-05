package metrics_test

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arasHi87/gogolook-task-api/internal/metrics"
	"github.com/arasHi87/gogolook-task-api/internal/outbox"
	"github.com/arasHi87/gogolook-task-api/internal/queue"
	"github.com/arasHi87/gogolook-task-api/internal/testenv"
)

// Depth and age are queried on scrape rather than written by a ticker, and the
// difference is not tidiness. A goroutine that writes gauges reports whatever
// it last saw: after a pause, a stall or a scrape gap it keeps publishing a
// stale number with a fresh timestamp, which is not a delay, it is a lie —
// and the case it lies in is exactly the one anyone is looking at it for.
func TestTheQueueCollectorReportsDepthAndAge(t *testing.T) {
	t.Parallel()

	pool := testenv.Postgres(t)
	enqueue(t, pool, "task.event", 3)

	r := newRegistry(t)
	if err := r.Register(metrics.NewQueueCollector(queue.NewStore(pool), nil)); err != nil {
		t.Fatalf("register: %v", err)
	}

	depth := gather(t, r, "job_queue_depth")
	if depth == nil {
		t.Fatal("job_queue_depth is missing")
	}
	if got := depth.GetMetric()[0].GetGauge().GetValue(); got != 3 {
		t.Errorf("depth = %v, want 3", got)
	}
	if got := labelsOf(depth.GetMetric()[0]); !strings.Contains(got, "state=available") {
		t.Errorf("labels = %q, want the available state", got)
	}

	// The signal to alert on. Depth alone is ambiguous — ten thousand jobs
	// that drain in twenty seconds is fine and five stuck for an hour is an
	// outage — and this is the one that tells them apart.
	age := gather(t, r, "job_queue_oldest_pending_age_seconds")
	if age == nil {
		t.Fatal("job_queue_oldest_pending_age_seconds is missing")
	}
	if got := age.GetMetric()[0].GetGauge().GetValue(); got < 0 {
		t.Errorf("age = %v, want a non-negative number", got)
	}
}

// Finished jobs are history, not backlog. Counting them would make the depth
// gauge climb forever until the purger ran, and the alert on it meaningless.
func TestTheQueueCollectorIgnoresFinishedJobs(t *testing.T) {
	t.Parallel()

	pool := testenv.Postgres(t)
	enqueue(t, pool, "task.event", 2)

	if _, err := pool.Exec(t.Context(),
		`UPDATE jobs SET state = 'succeeded', finalized_at = now()`); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	r := newRegistry(t)
	if err := r.Register(metrics.NewQueueCollector(queue.NewStore(pool), nil)); err != nil {
		t.Fatalf("register: %v", err)
	}

	if f := gather(t, r, "job_queue_depth"); f != nil && len(f.GetMetric()) > 0 {
		t.Errorf("succeeded jobs are counted as backlog: %v", seriesOf(f))
	}
}

// The pool is the resource that saturates first, and the symptom in the API
// metrics is latency with no obvious cause.
func TestThePoolCollectorReportsUtilisation(t *testing.T) {
	t.Parallel()

	pool := testenv.Postgres(t)

	r := newRegistry(t)
	if err := r.Register(metrics.NewPoolCollector(pool, "api")); err != nil {
		t.Fatalf("register: %v", err)
	}

	f := gather(t, r, "db_pool_connections")
	if f == nil {
		t.Fatal("db_pool_connections is missing")
	}

	states := map[string]bool{}
	for _, m := range f.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == "state" {
				states[l.GetValue()] = true
			}
		}
	}
	// max is what makes the others a ratio rather than a number nobody can
	// interpret.
	for _, want := range []string{"acquired", "idle", "total", "max"} {
		if !states[want] {
			t.Errorf("db_pool_connections has no %q state", want)
		}
	}
}

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
