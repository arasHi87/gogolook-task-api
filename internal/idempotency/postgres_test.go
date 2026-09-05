package idempotency_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arasHi87/gogolook-task-api/internal/idempotency"
	"github.com/arasHi87/gogolook-task-api/internal/task"
	"github.com/arasHi87/gogolook-task-api/internal/task/pgrepo"
	"github.com/arasHi87/gogolook-task-api/internal/testenv"
)

const ttl = time.Hour

func newStore(t *testing.T) (*idempotency.PostgresStore, *pgxpool.Pool) {
	t.Helper()
	pool := testenv.Postgres(t)
	return idempotency.NewPostgresStore(pool), pool
}

func fingerprintOf(body string) []byte {
	return idempotency.Fingerprint("POST", "/tasks", []byte(body))
}

// reserve fails the test if the key was already taken.
func reserve(t *testing.T, s *idempotency.PostgresStore, key, body string) idempotency.Unit {
	t.Helper()

	unit, seen, err := s.Reserve(t.Context(), key, fingerprintOf(body), ttl)
	if err != nil {
		t.Fatalf("Reserve(%q): %v", key, err)
	}
	if unit == nil {
		t.Fatalf("Reserve(%q) found an existing record %+v, want a reservation", key, seen)
	}
	return unit
}

func TestReserveThenCompleteStoresTheResponse(t *testing.T) {
	t.Parallel()

	s, _ := newStore(t)
	want := []byte(`{"id":"a"}`)

	if err := reserve(t, s, "key-1", `{"name":"a"}`).Complete(200, "application/json", want); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	unit, seen, err := s.Reserve(t.Context(), "key-1", fingerprintOf(`{"name":"a"}`), ttl)
	if err != nil {
		t.Fatalf("Reserve again: %v", err)
	}
	if unit != nil {
		t.Fatal("a completed key was reserved again; the request would execute twice")
	}
	switch {
	case !seen.Completed():
		t.Errorf("state = %q, want completed", seen.State)
	case seen.StatusCode != 200:
		t.Errorf("status = %d, want 200", seen.StatusCode)
	case seen.ContentType != "application/json":
		t.Errorf("content type = %q, want application/json", seen.ContentType)
	case !bytes.Equal(seen.Body, want):
		// Byte-identical, not merely equivalent. The column is bytea rather
		// than jsonb precisely so a replay is the response that was sent
		// rather than a re-rendering of it.
		t.Errorf("body = %s, want %s", seen.Body, want)
	}
}

// An abandoned key leaves nothing behind, so the client's retry starts clean
// rather than being refused a retry of something that never happened.
func TestAbandonLeavesNoKey(t *testing.T) {
	t.Parallel()

	s, _ := newStore(t)
	reserve(t, s, "key-1", `{"name":"a"}`).Abandon()

	unit, seen, err := s.Reserve(t.Context(), "key-1", fingerprintOf(`{"name":"a"}`), ttl)
	if err != nil {
		t.Fatalf("Reserve after abandon: %v", err)
	}
	if unit == nil {
		t.Fatalf("the abandoned key is still held: %+v", seen)
	}
	unit.Abandon()
}

// The claim this whole design exists for: the task, its outbox row and the
// stored response are one transaction. Either all three are there or none is,
// and there is no window in which a key promises a response for a task that
// does not exist.
func TestTheWriteAndTheKeyCommitTogether(t *testing.T) {
	t.Parallel()

	s, pool := newStore(t)
	repo := pgrepo.New(pool)

	t.Run("abandoned", func(t *testing.T) {
		unit := reserve(t, s, "rolled-back", `{"name":"a"}`)
		created, err := repo.Create(unit.Context(), &task.Task{ID: uuid.New(), Name: "rolled back"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		unit.Abandon()

		if got := countTasks(t, pool, created.ID); got != 0 {
			t.Errorf("%d task rows survived the abandon", got)
		}
		if got := countJobs(t, pool); got != 0 {
			t.Errorf("%d outbox rows survived the abandon", got)
		}
		if got := countKeys(t, pool, "rolled-back"); got != 0 {
			t.Errorf("%d key rows survived the abandon", got)
		}
	})

	t.Run("completed", func(t *testing.T) {
		unit := reserve(t, s, "committed", `{"name":"b"}`)
		created, err := repo.Create(unit.Context(), &task.Task{ID: uuid.New(), Name: "committed"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := unit.Complete(200, "application/json", []byte(`{"ok":true}`)); err != nil {
			t.Fatalf("Complete: %v", err)
		}

		if got := countTasks(t, pool, created.ID); got != 1 {
			t.Errorf("%d task rows after the commit, want 1", got)
		}
		if got := countJobs(t, pool); got != 1 {
			t.Errorf("%d outbox rows after the commit, want 1", got)
		}
		if got := countKeys(t, pool, "committed"); got != 1 {
			t.Errorf("%d key rows after the commit, want 1", got)
		}
	})
}

// Two requests arriving at once with the same key is the case the whole thing
// has to get right, and it is the one a check-then-insert gets wrong. The row
// lock makes the second wait for the first to commit and then see its result,
// rather than both deciding they are first.
func TestConcurrentReservationsSerialise(t *testing.T) {
	t.Parallel()

	s, _ := newStore(t)
	first := reserve(t, s, "key-1", `{"name":"a"}`)

	type result struct {
		unit idempotency.Unit
		seen *idempotency.Record
		err  error
	}
	second := make(chan result, 1)
	go func() {
		unit, seen, err := s.Reserve(context.Background(), "key-1", fingerprintOf(`{"name":"a"}`), ttl)
		second <- result{unit, seen, err}
	}()

	// The second reservation is now blocked on the row the first holds. It
	// unblocks when the first commits, not before.
	select {
	case got := <-second:
		t.Fatalf("the second reservation returned while the first was still open: %+v", got)
	case <-time.After(250 * time.Millisecond):
	}

	if err := first.Complete(201, "application/json", []byte(`{"id":"a"}`)); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	select {
	case got := <-second:
		switch {
		case got.err != nil:
			t.Fatalf("second Reserve: %v", got.err)
		case got.unit != nil:
			t.Fatal("both requests reserved the same key; the write would run twice")
		case got.seen.StatusCode != 201:
			t.Errorf("the second request saw status %d, want the first request's 201", got.seen.StatusCode)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second reservation never unblocked after the first committed")
	}
}

func TestPurgeRemovesExpiredKeys(t *testing.T) {
	t.Parallel()

	s, pool := newStore(t)

	// A TTL in the past: the row is written already expired, which is what a
	// key looks like a day after it was used.
	unit, _, err := s.Reserve(t.Context(), "stale", fingerprintOf(`{}`), -time.Hour)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := unit.Complete(200, "application/json", []byte(`{}`)); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	live := reserve(t, s, "fresh", `{}`)
	if err := live.Complete(200, "application/json", []byte(`{}`)); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	n, err := s.Purge(t.Context())
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d keys, want 1", n)
	}
	if got := countKeys(t, pool, "stale"); got != 0 {
		t.Error("the expired key survived the purge")
	}
	if got := countKeys(t, pool, "fresh"); got != 1 {
		t.Error("the purge took a key that had not expired")
	}
}

func countTasks(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) int {
	t.Helper()
	return count(t, pool, `SELECT count(*) FROM tasks WHERE id = $1`, id)
}

func countKeys(t *testing.T, pool *pgxpool.Pool, key string) int {
	t.Helper()
	return count(t, pool, `SELECT count(*) FROM idempotency_keys WHERE key = $1`, key)
}

func countJobs(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	return count(t, pool, `SELECT count(*) FROM jobs`)
}

func count(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()

	var n int
	if err := pool.QueryRow(t.Context(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}
