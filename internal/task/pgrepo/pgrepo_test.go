package pgrepo_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arasHi87/gogolook-task-api/internal/task"
	"github.com/arasHi87/gogolook-task-api/internal/task/pgrepo"
	"github.com/arasHi87/gogolook-task-api/internal/testenv"
)

func newRepo(t *testing.T) (*pgrepo.Repo, *pgxpool.Pool) {
	t.Helper()
	pool := testenv.Postgres(t)
	return pgrepo.New(pool), pool
}

func newService(t *testing.T) (*task.Service, *pgxpool.Pool) {
	t.Helper()
	repo, pool := newRepo(t)
	return task.NewService(repo), pool
}

func mustCreate(t *testing.T, svc *task.Service, name string, status task.Status) *task.Task {
	t.Helper()
	got, err := svc.Create(t.Context(), task.CreateInput{Name: name, Status: status})
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	return got
}

// countJobs returns how many queue rows exist, optionally for one event.
func countJobs(t *testing.T, pool *pgxpool.Pool, event string) int {
	t.Helper()

	const q = `SELECT count(*) FROM jobs WHERE $1 = '' OR payload->>'event' = $1`
	var n int
	if err := pool.QueryRow(t.Context(), q, event).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return n
}

// The claim the whole design rests on: either the task and its event both
// exist, or neither does. Both directions, because a scheme that only ever
// commits proves nothing.
func TestOutboxIsTransactional(t *testing.T) {
	t.Parallel()

	t.Run("commit writes both", func(t *testing.T) {
		svc, pool := newService(t)

		created := mustCreate(t, svc, "buy milk", task.StatusIncomplete)

		if got := countJobs(t, pool, pgrepo.EventCreated); got != 1 {
			t.Fatalf("got %d created events, want 1", got)
		}

		var payload pgrepo.Event
		var raw []byte
		if err := pool.QueryRow(t.Context(),
			`SELECT payload FROM jobs WHERE payload->>'event' = $1`, pgrepo.EventCreated).Scan(&raw); err != nil {
			t.Fatalf("read payload: %v", err)
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if payload.TaskID != created.ID {
			t.Errorf("event task_id = %s, want %s", payload.TaskID, created.ID)
		}
		// The event carries the state at the moment of the change, so a
		// handler does not have to re-read a row that may have moved on.
		if payload.Name != "buy milk" || payload.Version != created.Version {
			t.Errorf("event = %+v, want the task's state at creation", payload)
		}
	})

	t.Run("a failed write leaves neither", func(t *testing.T) {
		repo, pool := newRepo(t)

		// Through the repository, not the service: the service rejects an
		// over-long name before any SQL runs, so going through it would test
		// the validator rather than the transaction. Here the CHECK constraint
		// on tasks.name fails inside the transaction the event would be
		// written by.
		_, err := repo.Create(t.Context(), &task.Task{
			ID: uuid.New(), Name: longName(), Status: task.StatusIncomplete,
			Version: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		})
		if err == nil {
			t.Fatal("Create succeeded with a name the column forbids")
		}

		var tasks int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM tasks`).Scan(&tasks); err != nil {
			t.Fatalf("count tasks: %v", err)
		}
		if tasks != 0 {
			t.Errorf("got %d tasks after a failed create, want 0", tasks)
		}
		if got := countJobs(t, pool, ""); got != 0 {
			t.Errorf("got %d jobs after a failed create, want 0", got)
		}
	})
}

// longName exceeds both the domain's limit and the column's CHECK.
func longName() string {
	b := make([]byte, task.MaxNameLength+1)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

// Every state change produces exactly one event.
func TestEveryChangeEnqueuesOneEvent(t *testing.T) {
	t.Parallel()
	svc, pool := newService(t)

	created := mustCreate(t, svc, "original", task.StatusIncomplete)
	if _, err := svc.Update(t.Context(), task.UpdateInput{
		ID: created.ID, Name: "changed", Status: task.StatusCompleted,
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := svc.Delete(t.Context(), created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	for event, want := range map[string]int{
		pgrepo.EventCreated: 1,
		pgrepo.EventUpdated: 1,
		pgrepo.EventDeleted: 1,
	} {
		if got := countJobs(t, pool, event); got != want {
			t.Errorf("%s events = %d, want %d", event, got, want)
		}
	}
}

// The unique key includes the version, so two updates produce two events but a
// retried update produces one. Without it, a client retrying a PUT after a
// timeout delivers the same change twice.
func TestRetriedUpdateDoesNotDuplicateItsEvent(t *testing.T) {
	t.Parallel()
	svc, pool := newService(t)
	created := mustCreate(t, svc, "original", task.StatusIncomplete)

	// Two distinct updates: two events.
	for i := range 2 {
		if _, err := svc.Update(t.Context(), task.UpdateInput{
			ID: created.ID, Name: "change", Status: task.Status(i % 2),
		}); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	if got := countJobs(t, pool, pgrepo.EventUpdated); got != 2 {
		t.Fatalf("got %d update events for two updates, want 2", got)
	}
}

func TestCRUD(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t)

	created := mustCreate(t, svc, "buy milk", task.StatusIncomplete)
	if created.Version != 1 {
		t.Errorf("version = %d, want 1", created.Version)
	}

	got, err := svc.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "buy milk" || got.Status != task.StatusIncomplete {
		t.Errorf("got %+v, want the created values", got)
	}
	// Timestamps must come back in UTC, or the two backends disagree about a
	// value the cursor is built from.
	if got.CreatedAt.Location() != time.UTC {
		t.Errorf("created_at is in %v, want UTC", got.CreatedAt.Location())
	}

	updated, err := svc.Update(t.Context(), task.UpdateInput{
		ID: created.ID, Name: "buy oat milk", Status: task.StatusCompleted,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Version != 2 {
		t.Errorf("version = %d, want 2", updated.Version)
	}
	if !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("created_at changed on update: %v -> %v", created.CreatedAt, updated.CreatedAt)
	}

	if err := svc.Delete(t.Context(), created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(t.Context(), created.ID); !errors.Is(err, task.ErrNotFound) {
		t.Errorf("Get after delete = %v, want ErrNotFound", err)
	}
}

// The version guard is in the WHERE clause, so the check and the write are one
// statement. A read-then-write would have a window between them.
func TestOptimisticConcurrency(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t)
	created := mustCreate(t, svc, "original", task.StatusIncomplete)

	if _, err := svc.Update(t.Context(), task.UpdateInput{
		ID: created.ID, Name: "theirs", Status: task.StatusCompleted,
	}); err != nil {
		t.Fatalf("first update: %v", err)
	}

	_, err := svc.Update(t.Context(), task.UpdateInput{
		ID: created.ID, Name: "mine", Status: task.StatusIncomplete,
		ExpectedVersion: created.Version,
	})
	if !errors.Is(err, task.ErrConflict) {
		t.Fatalf("stale update = %v, want ErrConflict", err)
	}

	// A conflict and a missing row are different answers, and the client can
	// act on the difference.
	_, err = svc.Update(t.Context(), task.UpdateInput{
		ID: uuid.New(), Name: "x", Status: task.StatusIncomplete, ExpectedVersion: 1,
	})
	if !errors.Is(err, task.ErrNotFound) {
		t.Errorf("update of a missing task = %v, want ErrNotFound", err)
	}
}

// Concurrent conditional updates: exactly one may win, or the version is not
// doing its job.
func TestConcurrentUpdatesConflict(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t)
	created := mustCreate(t, svc, "contended", task.StatusIncomplete)

	const racers = 8
	results := make(chan error, racers)
	start := make(chan struct{})

	for i := range racers {
		go func(i int) {
			<-start
			_, err := svc.Update(context.Background(), task.UpdateInput{
				ID: created.ID, Name: "winner", Status: task.Status(i % 2),
				ExpectedVersion: created.Version,
			})
			results <- err
		}(i)
	}
	close(start)

	var won, conflicted int
	for range racers {
		switch err := <-results; {
		case err == nil:
			won++
		case errors.Is(err, task.ErrConflict):
			conflicted++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}

	if won != 1 {
		t.Errorf("%d writers won, want exactly 1", won)
	}
	if conflicted != racers-1 {
		t.Errorf("%d writers were told about the conflict, want %d", conflicted, racers-1)
	}
}

// Keyset pagination must visit every row exactly once, including when several
// share a created_at — the case an ordering without a tiebreak gets wrong.
func TestListPaginatesWithoutGapsOrRepeats(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t)

	const total = 25
	for i := range total {
		mustCreate(t, svc, "task", task.Status(i%2))
	}

	seen := map[uuid.UUID]int{}
	token := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		page, err := svc.List(t.Context(), task.ListInput{PageSize: 7, PageToken: token})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, tk := range page.Tasks {
			seen[tk.ID]++
		}
		if token = page.Next.Encode(); token == "" {
			break
		}
	}

	if len(seen) != total {
		t.Errorf("saw %d distinct tasks, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("task %s was returned %d times, want once", id, n)
		}
	}
}

func TestListFiltersByStatusAndOrdersNewestFirst(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t)

	for i := range 6 {
		mustCreate(t, svc, "task", task.Status(i%2))
		time.Sleep(time.Millisecond) // distinct created_at, so the order is observable
	}

	completed := task.StatusCompleted
	page, err := svc.List(t.Context(), task.ListInput{Status: &completed})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Tasks) != 3 {
		t.Fatalf("got %d completed tasks, want 3", len(page.Tasks))
	}
	for _, tk := range page.Tasks {
		if tk.Status != task.StatusCompleted {
			t.Errorf("filtered list returned %v", tk.Status)
		}
	}
	for i := 1; i < len(page.Tasks); i++ {
		if page.Tasks[i-1].CreatedAt.Before(page.Tasks[i].CreatedAt) {
			t.Errorf("list is not newest-first at %d", i)
		}
	}
}

// Status is narrowed to a smallint by the column, not by a Go conversion.
// A Go conversion would wrap silently; the column rejects the value.
func TestOutOfRangeStatusIsRejectedByTheColumn(t *testing.T) {
	t.Parallel()
	repo, pool := newRepo(t)

	// 32768 wraps to -32768 in an int16 and would be stored as a valid-looking
	// number if the narrowing happened in Go.
	_, err := repo.Create(t.Context(), &task.Task{
		ID: uuid.New(), Name: "out of range", Status: task.Status(32768),
		Version: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("a status outside smallint was accepted")
	}

	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM tasks`).Scan(&n); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if n != 0 {
		t.Errorf("got %d tasks, want 0: a wrapped value was stored", n)
	}
}
