package outbox_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arasHi87/gogolook-task-api/internal/outbox"
	"github.com/arasHi87/gogolook-task-api/internal/testenv"
)

// insertTask writes a domain row, so a transaction has both writes in it.
func insertTask(ctx context.Context, tx pgx.Tx, name string) (uuid.UUID, error) {
	id := uuid.New()
	const q = `INSERT INTO tasks (id, name, status, version) VALUES ($1, $2, 0, 1)`
	_, err := tx.Exec(ctx, q, id, name)
	return id, err
}

func counts(t *testing.T, pool *pgxpool.Pool) (tasks, jobs int) {
	t.Helper()
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM tasks`).Scan(&tasks); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM jobs`).Scan(&jobs); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return tasks, jobs
}

// The dual-write problem, eliminated rather than mitigated. Both directions,
// because a scheme that only ever commits proves nothing: there must be no
// window in which a task exists with no event queued, and none in which an
// event fires for a task that rolled back.
func TestBothOrNeither(t *testing.T) {
	t.Parallel()

	t.Run("commit", func(t *testing.T) {
		t.Parallel()
		pool := testenv.Postgres(t)

		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := insertTask(t.Context(), tx, "committed"); err != nil {
			t.Fatalf("insert task: %v", err)
		}
		if _, err := outbox.Enqueue(t.Context(), tx, outbox.Job{
			Kind: "task.event", Payload: map[string]string{"event": "task.created"},
		}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatalf("commit: %v", err)
		}

		if tasks, jobs := counts(t, pool); tasks != 1 || jobs != 1 {
			t.Errorf("after commit: %d tasks, %d jobs; want 1 and 1", tasks, jobs)
		}
	})

	t.Run("rollback", func(t *testing.T) {
		t.Parallel()
		pool := testenv.Postgres(t)

		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := insertTask(t.Context(), tx, "rolled back"); err != nil {
			t.Fatalf("insert task: %v", err)
		}
		if _, err := outbox.Enqueue(t.Context(), tx, outbox.Job{
			Kind: "task.event", Payload: map[string]string{"event": "task.created"},
		}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if err := tx.Rollback(t.Context()); err != nil {
			t.Fatalf("rollback: %v", err)
		}

		if tasks, jobs := counts(t, pool); tasks != 0 || jobs != 0 {
			t.Errorf("after rollback: %d tasks, %d jobs; want 0 and 0 — the event outlived its task", tasks, jobs)
		}
	})
}

// An uncommitted job must be invisible to everyone else, which is what stops a
// worker claiming an event for a task that has not been written yet.
func TestUncommittedJobsAreInvisible(t *testing.T) {
	t.Parallel()
	pool := testenv.Postgres(t)

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	//nolint:errcheck // rolled back explicitly below
	defer tx.Rollback(t.Context())

	if _, err := outbox.Enqueue(t.Context(), tx, outbox.Job{Kind: "task.event", Payload: 1}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// A different connection, mid-transaction.
	if _, jobs := counts(t, pool); jobs != 0 {
		t.Errorf("another connection sees %d uncommitted jobs, want 0", jobs)
	}
}

// The partial unique index is the whole design: a key is unique only while a
// job holding it is live, so the same logical event can be enqueued again once
// the previous one has finalized.
func TestUniqueKeyDeduplicatesLiveJobs(t *testing.T) {
	t.Parallel()
	pool := testenv.Postgres(t)

	enqueue := func(key string) bool {
		t.Helper()
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		inserted, err := outbox.Enqueue(t.Context(), tx, outbox.Job{
			Kind: "task.event", Payload: 1, UniqueKey: key,
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return inserted
	}

	if !enqueue("task.created:abc") {
		t.Fatal("the first enqueue was deduplicated")
	}
	// A duplicate is not an error: it is the deduplication working, and the
	// caller wants to know so it can say so in a metric.
	if enqueue("task.created:abc") {
		t.Error("a duplicate key was inserted while the first job is still live")
	}
	if _, jobs := counts(t, pool); jobs != 1 {
		t.Errorf("got %d jobs, want 1", jobs)
	}

	// Once the job finalizes it leaves the index's predicate, so the same
	// logical event can happen again.
	if _, err := pool.Exec(t.Context(),
		`UPDATE jobs SET state = 'succeeded', finalized_at = now()`); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if !enqueue("task.created:abc") {
		t.Error("the key stayed reserved after its job finalized")
	}
}

// An empty key means no deduplication at all, which is what a job that is
// genuinely allowed to repeat needs.
func TestEmptyUniqueKeyDoesNotDeduplicate(t *testing.T) {
	t.Parallel()
	pool := testenv.Postgres(t)

	for range 3 {
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		inserted, err := outbox.Enqueue(t.Context(), tx, outbox.Job{Kind: "task.event", Payload: 1})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if !inserted {
			t.Fatal("an unkeyed job was deduplicated")
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	if _, jobs := counts(t, pool); jobs != 3 {
		t.Errorf("got %d jobs, want 3", jobs)
	}
}

func TestEnqueueDefaults(t *testing.T) {
	t.Parallel()
	pool := testenv.Postgres(t)

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := outbox.Enqueue(t.Context(), tx, outbox.Job{Kind: "task.event", Payload: 1}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var (
		state       string
		attempt     int
		maxAttempts int
		uniqueKey   *string
		scheduledAt time.Time
	)
	const q = `SELECT state, attempt, max_attempts, unique_key, scheduled_at FROM jobs`
	if err := pool.QueryRow(t.Context(), q).Scan(&state, &attempt, &maxAttempts, &uniqueKey, &scheduledAt); err != nil {
		t.Fatalf("read job: %v", err)
	}

	if state != "available" {
		t.Errorf("state = %q, want available", state)
	}
	if attempt != 0 {
		t.Errorf("attempt = %d, want 0", attempt)
	}
	if maxAttempts != 5 {
		t.Errorf("max_attempts = %d, want the table default of 5", maxAttempts)
	}
	// An empty key must be stored as NULL, not as "": the partial unique index
	// would otherwise treat every unkeyed job as a duplicate of the last.
	if uniqueKey != nil {
		t.Errorf("unique_key = %q, want NULL for an unkeyed job", *uniqueKey)
	}
	if scheduledAt.IsZero() {
		t.Error("scheduled_at was not defaulted")
	}
}

func TestEnqueueRejectsAJobWithNoKind(t *testing.T) {
	t.Parallel()
	pool := testenv.Postgres(t)

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	//nolint:errcheck // nothing to commit
	defer tx.Rollback(t.Context())

	if _, err := outbox.Enqueue(t.Context(), tx, outbox.Job{Payload: 1}); err == nil {
		t.Error("a job with no kind was accepted; nothing could ever handle it")
	}
}

// The notification is best-effort and must never fail a write: the claim loop
// polls as well, so a lost one costs latency and nothing else.
func TestNotify(t *testing.T) {
	t.Parallel()
	pool := testenv.Postgres(t)

	if err := outbox.Notify(t.Context(), pool, "task.event"); err != nil {
		t.Errorf("Notify with nobody listening = %v, want nil", err)
	}
}
