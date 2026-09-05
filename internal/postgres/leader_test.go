package postgres_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/postgres"
	"github.com/arasHi87/gogolook-task-api/internal/testenv"
)

// Leader election lives here rather than beside the queue because it is a
// Postgres primitive, not a queue one: an advisory lock scoped to a
// transaction. The queue was its first caller and the idempotency purger is
// its second, and a shared namespace with one key derivation is the whole
// point.

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

			won, err := postgres.WithLeader(context.Background(), tx, "test-sweep", func(context.Context) error {
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

		won, err := postgres.WithLeader(t.Context(), tx, "test-sweep", func(context.Context) error { return nil })
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

	won, err := postgres.WithLeader(t.Context(), first, "sweep-a", func(context.Context) error { return nil })
	if err != nil || !won {
		t.Fatalf("first lock: won=%v err=%v", won, err)
	}

	second, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	//nolint:errcheck // rolled back at the end
	defer second.Rollback(t.Context())

	won, err = postgres.WithLeader(t.Context(), second, "sweep-b", func(context.Context) error { return nil })
	if err != nil {
		t.Fatalf("second lock: %v", err)
	}
	if !won {
		t.Error("a differently-named sweep was blocked; the keys collide")
	}
}
