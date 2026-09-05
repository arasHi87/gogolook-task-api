package queue_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/queue"
)

// The crash story. A worker that dies leaves its job in running with a lease
// that stops being extended; once it lapses the reaper hands the job back and
// another worker finishes it. Exactly once, still.
func TestExpiredLeaseIsReclaimedAndRerun(t *testing.T) {
	t.Parallel()
	pool := newPostgres(t)
	enqueue(t, pool, kind, 1)

	store := queue.NewStore(pool)
	cfg := testConfig()

	// A "dead" worker: it claims the job and never heartbeats or finishes.
	claimed, err := store.Claim(t.Context(), "dead-worker", 1, cfg.Lease.D())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d jobs, want 1", len(claimed))
	}

	if got := readJob(t, pool, claimed[0].ID); got.State != queue.StateRunning {
		t.Fatalf("state = %s, want running", got.State)
	}

	// A live pool now takes over once the lease lapses.
	var ran atomic.Int64
	_, events := runPool(t, pool, cfg, map[string]queue.HandlerFunc{
		kind: func(context.Context, *queue.Job) error {
			ran.Add(1)
			return nil
		},
	})

	done := awaitOutcome(t, events, queue.OutcomeSucceeded, 20*time.Second)
	if done.JobID != claimed[0].ID {
		t.Errorf("a different job ran: %d, want %d", done.JobID, claimed[0].ID)
	}
	// The reclaim is a retry, so this is the second attempt.
	if done.Attempt < 2 {
		t.Errorf("succeeded on attempt %d, want at least 2", done.Attempt)
	}
	if got := ran.Load(); got != 1 {
		t.Errorf("the handler ran %d times, want once", got)
	}

	row := readJob(t, pool, done.JobID)
	if row.State != queue.StateSucceeded {
		t.Errorf("state = %s, want succeeded", row.State)
	}
	// The history records that the lease expired, and which worker lost it.
	if len(row.Errors) == 0 {
		t.Error("nothing recorded why the job was reclaimed")
	}
	if len(row.AttemptedBy) != 2 || row.AttemptedBy[0] != "dead-worker" {
		t.Errorf("attempted_by = %v, want the dead worker first", row.AttemptedBy)
	}
}

// The detail everyone forgets. A zombie worker that comes back from a GC pause
// after its lease expired must not be able to finalize a job the reaper
// already requeued and someone else is running — that silently loses the
// second attempt's result.
func TestStaleWorkerCannotFinalizeAReclaimedJob(t *testing.T) {
	t.Parallel()
	pool := newPostgres(t)
	enqueue(t, pool, kind, 1)

	store := queue.NewStore(pool)
	cfg := testConfig()

	// The zombie claims, and holds its view of the job.
	zombie, err := store.Claim(t.Context(), "zombie", 1, cfg.Lease.D())
	if err != nil || len(zombie) != 1 {
		t.Fatalf("claim: %v (%d jobs)", err, len(zombie))
	}
	stale := zombie[0]

	// The lease lapses and the reaper hands the job back.
	backoff := queue.NewBackoff(cfg.Backoff)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("the lease never expired")
		}
		retryable, _, err := store.Reap(t.Context(), backoff, cfg.MaxAttempts)
		if err != nil {
			t.Fatalf("reap: %v", err)
		}
		if retryable == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond) //nolint:forbidigo // waiting on a wall-clock lease
	}

	// Someone else takes it. The reap set a backoff, so the scheduler has to
	// see the job come due before it is claimable — that gap is the point of
	// retryable being distinct from available.
	var live []*queue.Job
	deadline = time.Now().Add(10 * time.Second)
	for len(live) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the reclaimed job never became claimable")
		}
		if _, err := store.Schedule(t.Context()); err != nil {
			t.Fatalf("schedule: %v", err)
		}
		if live, err = store.Claim(t.Context(), "live", 1, cfg.Lease.D()); err != nil {
			t.Fatalf("second claim: %v", err)
		}
		if len(live) == 0 {
			time.Sleep(20 * time.Millisecond) //nolint:forbidigo // waiting on a wall-clock backoff
		}
	}

	// The zombie wakes up and tries to finish. Every completion path must
	// refuse it, because the claim it was made under no longer exists.
	for name, finalize := range map[string]func() (bool, error){
		"succeed": func() (bool, error) { return store.Succeed(t.Context(), "zombie", stale) },
		"retry":   func() (bool, error) { return store.Retry(t.Context(), "zombie", stale, 0, errors.New("late")) },
		"discard": func() (bool, error) { return store.Discard(t.Context(), "zombie", stale, errors.New("late")) },
		"cancel":  func() (bool, error) { return store.Cancel(t.Context(), "zombie", stale, errors.New("late")) },
		"snooze":  func() (bool, error) { return store.Snooze(t.Context(), "zombie", stale, time.Second) },
	} {
		held, err := finalize()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// Zero rows affected is not an error: it is the guard working.
		if held {
			t.Errorf("%s from a stale worker was accepted; the live attempt's result would be lost", name)
		}
	}

	// The live worker still owns it and can still finish.
	held, err := store.Succeed(t.Context(), "live", live[0])
	if err != nil {
		t.Fatalf("live succeed: %v", err)
	}
	if !held {
		t.Error("the live worker's completion was refused")
	}
	if got := readJob(t, pool, stale.ID).State; got != queue.StateSucceeded {
		t.Errorf("state = %s, want succeeded", got)
	}
}

// A heartbeat is positive proof of liveness, which is what lets a short lease
// coexist with a long job. A static rescue timeout cannot tell slow from dead.
func TestHeartbeatKeepsALongJobAlive(t *testing.T) {
	t.Parallel()
	pool := newPostgres(t)
	enqueue(t, pool, kind, 1)

	cfg := testConfig()
	// The job runs for several lease periods. Without heartbeats the reaper
	// would take it away mid-flight.
	jobRuns := 4 * cfg.Lease.D()

	var ran atomic.Int64
	_, events := runPool(t, pool, cfg, map[string]queue.HandlerFunc{
		kind: func(ctx context.Context, _ *queue.Job) error {
			ran.Add(1)
			select {
			case <-time.After(jobRuns):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})

	done := awaitOutcome(t, events, queue.OutcomeSucceeded, 30*time.Second)
	if done.Attempt != 1 {
		t.Errorf("succeeded on attempt %d, want 1: the job was reclaimed while it was still running", done.Attempt)
	}
	if got := ran.Load(); got != 1 {
		t.Errorf("the handler ran %d times, want once", got)
	}
}

// Losing a lease mid-flight cancels the handler's context. Continuing would
// only spend the dependency's capacity on work whose completion write the
// guard is going to refuse anyway.
func TestLosingALeaseCancelsTheHandler(t *testing.T) {
	t.Parallel()
	pool := newPostgres(t)
	enqueue(t, pool, kind, 1)

	cfg := testConfig()
	store := queue.NewStore(pool)

	var (
		started  = make(chan struct{})
		once     sync.Once
		observed atomic.Bool
	)

	_, events := runPool(t, pool, cfg, map[string]queue.HandlerFunc{
		kind: func(ctx context.Context, _ *queue.Job) error {
			once.Do(func() { close(started) })
			<-ctx.Done()
			observed.Store(true)
			return ctx.Err()
		},
	})

	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("the handler never started")
	}

	// Steal the job out from under the running worker, exactly as the reaper
	// would after a lease lapse.
	id := onlyJobID(t, pool)
	if _, err := pool.Exec(t.Context(),
		`UPDATE jobs SET locked_by = 'someone-else' WHERE id = $1`, id); err != nil {
		t.Fatalf("steal: %v", err)
	}
	_ = store

	select {
	case <-events:
	case <-time.After(20 * time.Second):
		t.Fatal("the worker never noticed it had lost the lease")
	}
	if !observed.Load() {
		t.Error("the handler's context was not cancelled")
	}
}
