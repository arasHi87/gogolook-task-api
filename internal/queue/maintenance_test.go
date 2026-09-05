package queue_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/queue"
	"github.com/arasHi87/gogolook-task-api/internal/testenv"
)

// Maintenance is fleet-wide work, not per-replica. Three replicas each running
// the same UPDATE on the same tick is three-way write contention on identical
// rows for one sweep's worth of work.
func TestOnlyTheLeaderSweeps(t *testing.T) {
	t.Parallel()
	pool := testenv.Postgres(t)

	const replicas = 5
	var (
		led   atomic.Int64
		start = make(chan struct{})
		wg    sync.WaitGroup
	)

	for range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			tx, err := pool.Begin(context.Background())
			if err != nil {
				t.Errorf("begin: %v", err)
				return
			}
			//nolint:errcheck // rolled back or committed below
			defer tx.Rollback(context.Background())

			won, err := queue.WithLeader(context.Background(), tx, "test-sweep", func(context.Context) error {
				led.Add(1)
				// Held long enough that every other replica has certainly
				// tried and been refused.
				time.Sleep(200 * time.Millisecond) //nolint:forbidigo // holding the lock is the point
				return nil
			})
			if err != nil {
				t.Errorf("WithLeader: %v", err)
				return
			}
			if won {
				_ = tx.Commit(context.Background())
			}
		}()
	}

	close(start)
	wg.Wait()

	if got := led.Load(); got != 1 {
		t.Errorf("%d replicas ran the sweep, want exactly 1", got)
	}
}

// The lock is transaction-scoped, so it is released by the commit rather than
// by a defer a panic could skip — and the next tick can win it.
func TestLeadershipIsReleasedAfterEachSweep(t *testing.T) {
	t.Parallel()
	pool := testenv.Postgres(t)

	for i := range 3 {
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatalf("begin: %v", err)
		}

		won, err := queue.WithLeader(t.Context(), tx, "test-sweep", func(context.Context) error { return nil })
		if err != nil {
			t.Fatalf("WithLeader: %v", err)
		}
		if !won {
			t.Fatalf("sweep %d did not win the lock; the previous one never released it", i)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
}

// Different names must not exclude each other. Advisory locks share one
// namespace across the database, which is why the key is derived from a name
// rather than picked by hand.
func TestDifferentSweepsDoNotBlockEachOther(t *testing.T) {
	t.Parallel()
	pool := testenv.Postgres(t)

	first, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	//nolint:errcheck // rolled back at the end
	defer first.Rollback(t.Context())

	won, err := queue.WithLeader(t.Context(), first, "sweep-a", func(context.Context) error { return nil })
	if err != nil || !won {
		t.Fatalf("first lock: won=%v err=%v", won, err)
	}

	second, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	//nolint:errcheck // rolled back at the end
	defer second.Rollback(t.Context())

	won, err = queue.WithLeader(t.Context(), second, "sweep-b", func(context.Context) error { return nil })
	if err != nil {
		t.Fatalf("second lock: %v", err)
	}
	if !won {
		t.Error("a differently-named sweep was blocked; the keys collide")
	}
}

// A queue table that never deletes grows forever and takes its indexes with
// it, and the claim query slows down in proportion to history it will never
// read.
func TestPurgeRemovesFinalizedJobsPastRetention(t *testing.T) {
	t.Parallel()
	pool := testenv.Postgres(t)
	store := queue.NewStore(pool)

	// Three finalized jobs of different ages and states, and one still live.
	seed := []struct {
		state     string
		finalized string
	}{
		{"succeeded", "now() - interval '48 hours'"},
		{"succeeded", "now() - interval '1 hour'"},
		{"discarded", "now() - interval '48 hours'"},
	}
	for _, s := range seed {
		if _, err := pool.Exec(t.Context(),
			`INSERT INTO jobs (kind, payload, state, finalized_at) VALUES ($1, '{}', $2::job_state, `+s.finalized+`)`,
			kind, s.state); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	enqueue(t, pool, kind, 1)

	// Succeeded rows older than a day go; discarded rows are kept a week,
	// because they are the ones someone still has to look at.
	n, err := store.Purge(t.Context(), 24*time.Hour, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d jobs, want 1", n)
	}

	var remaining int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM jobs`).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 3 {
		t.Errorf("%d jobs remain, want 3: the recent success, the discarded one, and the live one", remaining)
	}

	// A live job is never purged, whatever its age.
	var live int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM jobs WHERE state = 'available'`).Scan(&live); err != nil {
		t.Fatalf("count live: %v", err)
	}
	if live != 1 {
		t.Errorf("%d live jobs remain, want 1", live)
	}
}

// The scheduler is what separates "is it due" from "is it stuck". A job whose
// backoff has not elapsed must stay put.
func TestSchedulerOnlyReleasesDueJobs(t *testing.T) {
	t.Parallel()
	pool := testenv.Postgres(t)
	store := queue.NewStore(pool)

	if _, err := pool.Exec(t.Context(),
		`INSERT INTO jobs (kind, payload, state, scheduled_at) VALUES
		   ($1, '{}', 'retryable', now() - interval '1 minute'),
		   ($1, '{}', 'retryable', now() + interval '1 hour'),
		   ($1, '{}', 'scheduled', now() - interval '1 minute')`, kind); err != nil {
		t.Fatalf("seed: %v", err)
	}

	n, err := store.Schedule(t.Context())
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if n != 2 {
		t.Errorf("made %d jobs available, want 2", n)
	}

	var notYet int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM jobs WHERE state = 'retryable'`).Scan(&notYet); err != nil {
		t.Fatalf("count: %v", err)
	}
	if notYet != 1 {
		t.Errorf("%d jobs are still retryable, want 1: a job whose backoff has not elapsed was released", notYet)
	}
}

// The configured maximum is a ceiling over the per-job budget, so an operator
// can turn retries down mid-incident. Without it the knob would silently do
// nothing, because the column default is what a producer gets by default.
func TestConfiguredMaxAttemptsIsACeiling(t *testing.T) {
	t.Parallel()
	pool := testenv.Postgres(t)
	enqueue(t, pool, kind, 1) // the row gets the column default of 5

	cfg := testConfig()
	cfg.MaxAttempts = 2

	var attempts atomic.Int64
	_, events := runPool(t, pool, cfg, map[string]queue.HandlerFunc{
		kind: func(context.Context, *queue.Job) error {
			attempts.Add(1)
			return context.DeadlineExceeded // retryable
		},
	})

	e := awaitOutcome(t, events, queue.OutcomeDiscarded, 30*time.Second)
	if e.Attempt != 2 {
		t.Errorf("discarded on attempt %d, want the configured ceiling of 2", e.Attempt)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("the handler ran %d times, want 2", got)
	}

	var maxAttempts int
	if err := pool.QueryRow(t.Context(), `SELECT max_attempts FROM jobs WHERE id = $1`, e.JobID).Scan(&maxAttempts); err != nil {
		t.Fatalf("read max_attempts: %v", err)
	}
	if maxAttempts != 5 {
		t.Errorf("the row still says %d, want 5: the ceiling must not rewrite the per-job budget", maxAttempts)
	}
}

var _ = config.Defaults
