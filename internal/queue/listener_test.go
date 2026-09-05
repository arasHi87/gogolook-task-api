package queue_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/outbox"
	"github.com/arasHi87/gogolook-task-api/internal/queue"
	"github.com/arasHi87/gogolook-task-api/internal/testenv"
)

// The notify is the latency optimisation and the poll is the guarantee. This
// is the first half: a job enqueued now is picked up in milliseconds rather
// than at the next tick.
func TestNotificationWakesTheClaimLoop(t *testing.T) {
	t.Parallel()

	dsn := testenv.DSN(t)
	pool := testenv.PostgresAt(t, dsn)

	listener := queue.NewListener(dsn, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listening := make(chan struct{})
	go func() {
		close(listening)
		_ = listener.Run(ctx)
	}()
	<-listening

	// A poll interval far longer than the test's patience, so anything that
	// arrives promptly can only have come from the notification.
	cfg := testConfig()
	cfg.PollInterval = config.Duration(30 * time.Second)

	p, err := queue.NewPool(queue.PoolOptions{
		Store:    queue.NewStore(pool),
		Handlers: map[string]queue.HandlerFunc{kind: func(context.Context, *queue.Job) error { return nil }},
		WorkerID: "notify-worker",
		Config:   cfg,
		Logger:   testLogger(),
		Notify:   listener.Notifications(),
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	events, unsubscribe := p.Subscribe(16)
	defer unsubscribe()

	done := make(chan struct{})
	go func() { defer close(done); _ = p.Run(ctx) }()

	// Let the first poll pass and the listener attach.
	time.Sleep(500 * time.Millisecond) //nolint:forbidigo // letting the initial poll settle

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := outbox.Enqueue(t.Context(), tx, outbox.Job{Kind: kind, Payload: 1}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := outbox.Notify(t.Context(), pool, kind); err != nil {
		t.Fatalf("notify: %v", err)
	}

	select {
	case e := <-events:
		if e.Outcome != queue.OutcomeSucceeded {
			t.Errorf("outcome = %s", e.Outcome)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the job was not picked up; with a 30s poll interval only the notification could have woken it")
	}

	cancel()
	<-done
}

// The second half: a notification that is never delivered costs latency and
// nothing else, because the ticker is what turns best-effort wake-ups into
// at-least-once pickup.
func TestPollingPicksUpWhatNotifyMisses(t *testing.T) {
	t.Parallel()
	pool := testenv.Postgres(t)

	// No listener at all, so nothing is ever notified.
	cfg := testConfig()
	enqueue(t, pool, kind, 3)

	var ran atomic.Int64
	_, events := runPool(t, pool, cfg, map[string]queue.HandlerFunc{
		kind: func(context.Context, *queue.Job) error {
			ran.Add(1)
			return nil
		},
	})

	await(t, events, 3, 20*time.Second)
	if got := ran.Load(); got != 3 {
		t.Errorf("%d jobs ran, want 3", got)
	}
}

// A listener that cannot connect must not take the process down: the poll is
// still the guarantee, and the listener retries.
func TestListenerSurvivesAnUnreachableDatabase(t *testing.T) {
	t.Parallel()

	listener := queue.NewListener("postgres://nobody@127.0.0.1:1/nowhere?sslmode=disable", testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- listener.Run(ctx) }()

	select {
	case err := <-done:
		// Run returns the context error so the reason travels with it. What
		// matters is that it is a cancellation and not a connection failure:
		// an unreachable database is retried, never fatal.
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Run = %v, want a cancellation", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return when its context was cancelled")
	}
}
