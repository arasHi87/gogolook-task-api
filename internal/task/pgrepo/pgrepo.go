// Package pgrepo is the Postgres task repository.
//
// Every write goes through a transaction that also enqueues the event it
// produced. That is the whole reason storage is not just a map: either the
// task and its event both exist, or neither does.
package pgrepo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arasHi87/gogolook-task-api/internal/outbox"
	"github.com/arasHi87/gogolook-task-api/internal/task"
)

// Repo implements task.Repository over Postgres.
type Repo struct {
	pool *pgxpool.Pool
}

// New returns a repository over pool.
func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

var _ task.Repository = (*Repo)(nil)

// Create stores a task and its event in one transaction.
func (r *Repo) Create(ctx context.Context, t *task.Task) (*task.Task, error) {
	return r.write(ctx, EventCreated, func(ctx context.Context, tx pgx.Tx) (*task.Task, error) {
		const q = `
			INSERT INTO tasks (id, name, status, version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING id, name, status, version, created_at, updated_at`

		row := tx.QueryRow(ctx, q, t.ID, t.Name, int32(t.Status), t.Version, t.CreatedAt, t.UpdatedAt)
		created, err := scanTask(row)
		if err != nil {
			return nil, fmt.Errorf("insert task: %w", err)
		}
		return created, nil
	})
}

// Update replaces a task and enqueues the change, optionally conditional on
// the version the caller read.
func (r *Repo) Update(ctx context.Context, t *task.Task, expectedVersion int64) (*task.Task, error) {
	return r.write(ctx, EventUpdated, func(ctx context.Context, tx pgx.Tx) (*task.Task, error) {
		// created_at is immutable and version is the database's to bump, not
		// the caller's to supply. The version guard is in the WHERE clause, so
		// the check and the write are one atomic statement — a read-then-write
		// would have a window between them.
		const q = `
			UPDATE tasks
			   SET name = $2, status = $3, updated_at = $4, version = version + 1
			 WHERE id = $1
			   AND ($5::bigint = 0 OR version = $5)
			RETURNING id, name, status, version, created_at, updated_at`

		row := tx.QueryRow(ctx, q, t.ID, t.Name, int32(t.Status), t.UpdatedAt, expectedVersion)
		updated, err := scanTask(row)
		if errors.Is(err, pgx.ErrNoRows) {
			// No row matched, which is either "no such task" or "the version
			// moved". They are different answers to the client, so ask.
			return nil, r.classifyMiss(ctx, tx, t.ID)
		}
		if err != nil {
			return nil, fmt.Errorf("update task: %w", err)
		}
		return updated, nil
	})
}

// Delete removes a task and enqueues the deletion.
func (r *Repo) Delete(ctx context.Context, id uuid.UUID) error {
	_, err := r.write(ctx, EventDeleted, func(ctx context.Context, tx pgx.Tx) (*task.Task, error) {
		const q = `
			DELETE FROM tasks
			 WHERE id = $1
			RETURNING id, name, status, version, created_at, updated_at`

		row := tx.QueryRow(ctx, q, id)
		deleted, err := scanTask(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, task.ErrNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("delete task: %w", err)
		}
		return deleted, nil
	})
	return err
}

// Get returns one task. It takes no transaction: there is nothing to enqueue.
func (r *Repo) Get(ctx context.Context, id uuid.UUID) (*task.Task, error) {
	const q = `
		SELECT id, name, status, version, created_at, updated_at
		  FROM tasks
		 WHERE id = $1`

	t, err := scanTask(r.pool.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, task.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	return t, nil
}

// List returns a page of tasks, newest first.
func (r *Repo) List(ctx context.Context, q task.ListQuery) (task.Page, error) {
	// The keyset predicate is a row comparison rather than two OR'd
	// conditions, because the row form is what the planner can turn into a
	// single index seek on (created_at DESC, id DESC).
	//
	// One row past the limit is fetched to learn whether another page exists,
	// which is cheaper than a second count query and always consistent with
	// the page just returned.
	const sql = `
		SELECT id, name, status, version, created_at, updated_at
		  FROM tasks
		 WHERE ($1::smallint IS NULL OR status = $1)
		   AND ($2::timestamptz IS NULL OR (created_at, id) < ($2, $3))
		 ORDER BY created_at DESC, id DESC
		 LIMIT $4`

	rows, err := r.pool.Query(ctx, sql, statusArg(q.Status), cursorTime(q.Cursor), cursorID(q.Cursor), q.Limit+1)
	if err != nil {
		return task.Page{}, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	tasks := make([]*task.Task, 0, q.Limit+1)
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return task.Page{}, fmt.Errorf("scan task: %w", err)
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return task.Page{}, fmt.Errorf("list tasks: %w", err)
	}

	page := task.Page{Tasks: tasks}
	if len(tasks) > q.Limit {
		page.Tasks = tasks[:q.Limit]
		page.Next = task.CursorOf(page.Tasks[len(page.Tasks)-1])
	}
	return page, nil
}

// write runs fn in a transaction and enqueues the event it produced, then
// notifies after the commit.
//
// The notification is deliberately outside the transaction. Sent inside, it
// would still be delivered at commit — but a worker woken by a transaction
// that then rolled back finds nothing, and one woken before the row is visible
// is woken for no reason.
func (r *Repo) write(
	ctx context.Context,
	event string,
	fn func(context.Context, pgx.Tx) (*task.Task, error),
) (*task.Task, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	//nolint:errcheck // a rollback after a successful commit is a no-op
	defer tx.Rollback(ctx)

	t, err := fn(ctx, tx)
	if err != nil {
		return nil, err
	}

	payload := eventFor(event, t)
	if _, err := outbox.Enqueue(ctx, tx, outbox.Job{
		Kind:      JobKind,
		Payload:   payload,
		UniqueKey: payload.ID(),
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	// Best-effort: the claim loop polls as well, so a lost notification costs
	// latency and nothing else. Failing the write for it would be worse.
	_ = outbox.Notify(ctx, r.pool, JobKind)

	return t, nil
}

// classifyMiss answers why an update matched no row.
func (r *Repo) classifyMiss(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	var exists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM tasks WHERE id = $1)", id).Scan(&exists); err != nil {
		return fmt.Errorf("classify update miss: %w", err)
	}
	if exists {
		return task.ErrConflict
	}
	return task.ErrNotFound
}

// row is what both QueryRow and Rows satisfy, so one scan serves both.
type row interface {
	Scan(dest ...any) error
}

func scanTask(r row) (*task.Task, error) {
	var (
		t task.Task
		// Scanned as int32 rather than int16: narrowing in Go would wrap
		// silently, while the smallint column rejects an out-of-range value
		// outright. The range check belongs where it is authoritative.
		status int32
	)
	if err := r.Scan(&t.ID, &t.Name, &status, &t.Version, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	t.Status = task.Status(status)
	// Postgres returns timestamptz in the session's zone; the domain works in
	// UTC, and a repository that returns local time makes two backends differ.
	t.CreatedAt = t.CreatedAt.UTC()
	t.UpdatedAt = t.UpdatedAt.UTC()
	return &t, nil
}

// statusArg renders an optional status filter as a nullable argument.
func statusArg(s *task.Status) *int32 {
	if s == nil {
		return nil
	}
	v := int32(*s)
	return &v
}

func cursorTime(c task.Cursor) *time.Time {
	if c.IsZero() {
		return nil
	}
	return &c.CreatedAt
}

func cursorID(c task.Cursor) *uuid.UUID {
	if c.IsZero() {
		return nil
	}
	return &c.ID
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// Referencing pgconn keeps the import that outbox.Querier is satisfied
// through, so the pool can be passed to Notify.
var _ interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
} = (*pgxpool.Pool)(nil)
