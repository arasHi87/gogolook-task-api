package queue_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/queue"
	"github.com/arasHi87/gogolook-task-api/internal/testenv"
)

// A clean shutdown finishes what it started. Only a crash should cost a lease
// period.
func TestStopWaitsForInflightWork(t *testing.T) {
	t.Parallel()
	pool := testenv.Postgres(t)
	enqueue(t, pool, kind, 1)

	cfg := testConfig()
	store := queue.NewStore(pool)

	var (
		started  = make(chan struct{})
		once     sync.Once
		finished atomic.Bool
	)
	p, err := queue.NewPool(queue.PoolOptions{
		Store: store,
		Handlers: map[string]queue.HandlerFunc{
			kind: func(context.Context, *queue.Job) error {
				once.Do(func() { close(started) })
				time.Sleep(300 * time.Millisecond) //nolint:forbidigo // simulating real work
				finished.Store(true)
				return nil
			},
		},
		WorkerID: "drain-worker",
		Config:   cfg,
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { defer close(done); _ = p.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("the handler never started")
	}

	// Stop is called before the run context is cancelled, exactly as the
	// composition root's drain does it.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if !finished.Load() {
		t.Error("Stop returned while a job was still running")
	}
	cancel()
	<-done

	if got := readJob(t, pool, onlyJobID(t, pool)).State; got != queue.StateSucceeded {
		t.Errorf("state = %s, want succeeded", got)
	}
}

// When the grace period runs out, whatever is still running is handed back
// explicitly rather than left to have its lease expire. A rolling deploy
// should not make jobs invisible for a lease period for no reason.
func TestStopReleasesWhatItCannotFinish(t *testing.T) {
	t.Parallel()
	pool := testenv.Postgres(t)
	enqueue(t, pool, kind, 1)

	cfg := testConfig()

	var (
		started = make(chan struct{})
		once    sync.Once
		release = make(chan struct{})
	)
	p, err := queue.NewPool(queue.PoolOptions{
		Store: queue.NewStore(pool),
		Handlers: map[string]queue.HandlerFunc{
			kind: func(ctx context.Context, _ *queue.Job) error {
				once.Do(func() { close(started) })
				select {
				case <-release:
				case <-ctx.Done():
				}
				return ctx.Err()
			},
		},
		WorkerID: "slow-worker",
		Config:   cfg,
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = p.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("the handler never started")
	}

	// A grace period the job cannot possibly meet.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer stopCancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	row := readJob(t, pool, onlyJobID(t, pool))
	if row.State != queue.StateAvailable {
		t.Errorf("state = %s, want available: an unfinished job must be handed back, not abandoned", row.State)
	}
	// The attempt is given back too, so a restart costs the job nothing.
	if row.Attempt != 0 {
		t.Errorf("attempt = %d after release, want 0", row.Attempt)
	}
	if row.LockedBy != nil {
		t.Errorf("locked_by = %q after release, want NULL", *row.LockedBy)
	}

	cancel()
	<-done
}

// Once draining, no new work is claimed. Otherwise a shutdown could start a
// job it has no time to finish.
func TestStopStopsClaiming(t *testing.T) {
	t.Parallel()
	pool := testenv.Postgres(t)
	enqueue(t, pool, kind, 20)

	cfg := testConfig()
	cfg.Workers = 1
	cfg.ClaimBatch = 1

	var ran atomic.Int64
	p, err := queue.NewPool(queue.PoolOptions{
		Store: queue.NewStore(pool),
		Handlers: map[string]queue.HandlerFunc{
			kind: func(context.Context, *queue.Job) error {
				ran.Add(1)
				return nil
			},
		},
		WorkerID: "one-worker",
		Config:   cfg,
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = p.Run(ctx) }()

	// Let it get going, then stop.
	deadline := time.Now().Add(15 * time.Second)
	for ran.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no job ever ran")
		}
		time.Sleep(10 * time.Millisecond) //nolint:forbidigo // waiting for the pool to start
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	after := ran.Load()

	cancel()
	<-done

	if got := ran.Load(); got != after {
		t.Errorf("%d jobs ran after Stop returned, want none", got-after)
	}

	var left int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM jobs WHERE state = 'available'`).Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	if left == 0 {
		t.Error("every job was claimed; draining did not stop the claim loop")
	}
}
