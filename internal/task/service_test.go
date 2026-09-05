package task_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/arasHi87/gogolook-task-api/internal/apperr"
	"github.com/arasHi87/gogolook-task-api/internal/task"
	"github.com/arasHi87/gogolook-task-api/internal/task/memrepo"
)

// newService returns a service over an empty in-memory store with a clock the
// test drives, so ordering assertions do not depend on how fast the machine is.
func newService(t *testing.T) (*task.Service, *memrepo.Store, *time.Time) {
	t.Helper()

	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	store := memrepo.New()
	svc := task.NewService(store, task.WithClock(func() time.Time { return now }))
	return svc, store, &now
}

func mustCreate(t *testing.T, svc *task.Service, name string, status task.Status) *task.Task {
	t.Helper()
	got, err := svc.Create(context.Background(), task.CreateInput{Name: name, Status: status})
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	return got
}

func TestCreateAssignsServerOwnedFields(t *testing.T) {
	t.Parallel()
	svc, _, now := newService(t)

	got := mustCreate(t, svc, "buy milk", task.StatusIncomplete)

	if got.ID == uuid.Nil {
		t.Error("Create did not assign an id")
	}
	if got.Name != "buy milk" || got.Status != task.StatusIncomplete {
		t.Errorf("got %+v, want the submitted name and status", got)
	}
	if !got.CreatedAt.Equal(*now) || !got.UpdatedAt.Equal(*now) {
		t.Errorf("timestamps = %v/%v, want both %v", got.CreatedAt, got.UpdatedAt, *now)
	}
	if got.Version != 1 {
		t.Errorf("version = %d, want 1", got.Version)
	}
}

// The domain validates on its own account, not because something upstream
// happened to check first. These are the same rules the proto declares.
func TestCreateValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		in        task.CreateInput
		wantField string
	}{
		{"empty name", task.CreateInput{Name: ""}, "name"},
		{"whitespace name", task.CreateInput{Name: "   "}, "name"},
		{"name too long", task.CreateInput{Name: strings.Repeat("x", 256)}, "name"},
		{"status out of range", task.CreateInput{Name: "ok", Status: 2}, "status"},
		{"negative status", task.CreateInput{Name: "ok", Status: -1}, "status"},
		{"name at the limit", task.CreateInput{Name: strings.Repeat("x", 255)}, ""},
		{"completed is fine", task.CreateInput{Name: "ok", Status: task.StatusCompleted}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, _, _ := newService(t)

			_, err := svc.Create(context.Background(), tc.in)
			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("Create = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, task.ErrInvalidArgument) {
				t.Fatalf("Create = %v, want ErrInvalidArgument", err)
			}
			if got := apperr.FieldOf(err); got != tc.wantField {
				t.Errorf("field = %q, want %q", got, tc.wantField)
			}
		})
	}
}

func TestUpdateReplacesAndBumpsVersion(t *testing.T) {
	t.Parallel()
	svc, _, now := newService(t)
	created := mustCreate(t, svc, "buy milk", task.StatusIncomplete)

	*now = now.Add(time.Hour)
	got, err := svc.Update(context.Background(), task.UpdateInput{
		ID: created.ID, Name: "buy oat milk", Status: task.StatusCompleted,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	if got.Name != "buy oat milk" || got.Status != task.StatusCompleted {
		t.Errorf("got %+v, want the replacement values", got)
	}
	if got.Version != 2 {
		t.Errorf("version = %d, want 2", got.Version)
	}
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("created_at = %v, want it immutable at %v", got.CreatedAt, created.CreatedAt)
	}
	if !got.UpdatedAt.After(created.UpdatedAt) {
		t.Errorf("updated_at = %v, want it to have moved past %v", got.UpdatedAt, created.UpdatedAt)
	}
}

// Optimistic concurrency: a caller that sends the version it read is told about
// a concurrent edit rather than silently overwriting it.
func TestUpdateOptimisticConcurrency(t *testing.T) {
	t.Parallel()
	svc, _, _ := newService(t)
	created := mustCreate(t, svc, "original", task.StatusIncomplete)

	// Someone else updates first.
	if _, err := svc.Update(context.Background(), task.UpdateInput{
		ID: created.ID, Name: "theirs", Status: task.StatusCompleted,
	}); err != nil {
		t.Fatalf("first update: %v", err)
	}

	_, err := svc.Update(context.Background(), task.UpdateInput{
		ID: created.ID, Name: "mine", Status: task.StatusIncomplete,
		ExpectedVersion: created.Version,
	})
	if !errors.Is(err, task.ErrConflict) {
		t.Fatalf("Update with a stale version = %v, want ErrConflict", err)
	}

	// Without a version, last write wins — which is what a client that never
	// read the task actually wants.
	if _, err := svc.Update(context.Background(), task.UpdateInput{
		ID: created.ID, Name: "mine", Status: task.StatusIncomplete,
	}); err != nil {
		t.Fatalf("unconditional Update = %v, want it to succeed", err)
	}
}

func TestNotFound(t *testing.T) {
	t.Parallel()
	svc, _, _ := newService(t)
	missing := uuid.New()

	if _, err := svc.Get(context.Background(), missing); !errors.Is(err, task.ErrNotFound) {
		t.Errorf("Get = %v, want ErrNotFound", err)
	}
	if err := svc.Delete(context.Background(), missing); !errors.Is(err, task.ErrNotFound) {
		t.Errorf("Delete = %v, want ErrNotFound", err)
	}
	_, err := svc.Update(context.Background(), task.UpdateInput{ID: missing, Name: "x"})
	if !errors.Is(err, task.ErrNotFound) {
		t.Errorf("Update = %v, want ErrNotFound", err)
	}
}

func TestDeleteIsNotIdempotent(t *testing.T) {
	t.Parallel()
	svc, store, _ := newService(t)
	created := mustCreate(t, svc, "temporary", task.StatusIncomplete)

	if err := svc.Delete(context.Background(), created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if store.Len() != 0 {
		t.Errorf("store holds %d tasks after delete, want 0", store.Len())
	}
	// Deliberate: the specification is silent, and telling the caller their id
	// was wrong is more useful than pretending the delete worked.
	if err := svc.Delete(context.Background(), created.ID); !errors.Is(err, task.ErrNotFound) {
		t.Errorf("second Delete = %v, want ErrNotFound", err)
	}
}

func TestListOrdersNewestFirst(t *testing.T) {
	t.Parallel()
	svc, _, now := newService(t)

	var want []string
	for _, name := range []string{"first", "second", "third"} {
		mustCreate(t, svc, name, task.StatusIncomplete)
		*now = now.Add(time.Minute)
		want = append([]string{name}, want...)
	}

	page, err := svc.List(context.Background(), task.ListInput{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var got []string
	for _, tk := range page.Tasks {
		got = append(got, tk.Name)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("List order = %v, want %v (newest first)", got, want)
	}
}

func TestListFiltersByStatus(t *testing.T) {
	t.Parallel()
	svc, _, now := newService(t)

	for i, s := range []task.Status{
		task.StatusIncomplete, task.StatusCompleted, task.StatusCompleted,
	} {
		mustCreate(t, svc, string(rune('a'+i)), s)
		*now = now.Add(time.Minute)
	}

	completed := task.StatusCompleted
	page, err := svc.List(context.Background(), task.ListInput{Status: &completed})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(page.Tasks))
	}
	for _, tk := range page.Tasks {
		if tk.Status != task.StatusCompleted {
			t.Errorf("filtered list returned %v", tk.Status)
		}
	}
}

// Keyset pagination must visit every task exactly once, even when several share
// a created_at — the case where an ordering without a tiebreak silently skips
// or repeats rows.
func TestListPaginatesWithoutGapsOrRepeats(t *testing.T) {
	t.Parallel()
	svc, _, now := newService(t)

	const total = 25
	for i := range total {
		mustCreate(t, svc, string(rune('a'+i%26)), task.StatusIncomplete)
		if i%2 == 0 { // half the tasks share a timestamp with their neighbour
			*now = now.Add(time.Minute)
		}
	}

	seen := map[uuid.UUID]int{}
	token := ""
	pages := 0
	for {
		page, err := svc.List(context.Background(), task.ListInput{PageSize: 7, PageToken: token})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		pages++
		for _, tk := range page.Tasks {
			seen[tk.ID]++
		}
		token = page.Next.Encode()
		if token == "" {
			break
		}
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}

	if len(seen) != total {
		t.Errorf("saw %d distinct tasks across %d pages, want %d", len(seen), pages, total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("task %s was returned %d times, want once", id, n)
		}
	}
}

func TestListPageSizePolicy(t *testing.T) {
	t.Parallel()
	svc, _, _ := newService(t)
	for range 30 {
		mustCreate(t, svc, "task", task.StatusIncomplete)
	}

	t.Run("zero means the default", func(t *testing.T) {
		page, err := svc.List(context.Background(), task.ListInput{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(page.Tasks) != task.DefaultPageSize {
			t.Errorf("got %d tasks, want the default %d", len(page.Tasks), task.DefaultPageSize)
		}
	})

	t.Run("above the maximum is refused, not clamped", func(t *testing.T) {
		// Silently clamping makes a client's paging maths wrong without telling
		// it, which is worse than a 400.
		_, err := svc.List(context.Background(), task.ListInput{PageSize: task.MaxPageSize + 1})
		if !errors.Is(err, task.ErrInvalidArgument) {
			t.Errorf("List = %v, want ErrInvalidArgument", err)
		}
	})

	t.Run("negative is refused", func(t *testing.T) {
		_, err := svc.List(context.Background(), task.ListInput{PageSize: -1})
		if !errors.Is(err, task.ErrInvalidArgument) {
			t.Errorf("List = %v, want ErrInvalidArgument", err)
		}
	})
}

// A page token the client did not get from us is a client error. Silently
// restarting at page one is how a paginating client ends up in a loop.
func TestListRejectsAnUnparseableToken(t *testing.T) {
	t.Parallel()
	svc, _, _ := newService(t)

	for _, token := range []string{"not-base64!!", "bm90LWEtY3Vyc29y", "MjAyNi0wOS0wNXxub3QtYS11dWlk"} {
		_, err := svc.List(context.Background(), task.ListInput{PageToken: token})
		if !errors.Is(err, task.ErrInvalidArgument) {
			t.Errorf("List(token=%q) = %v, want ErrInvalidArgument", token, err)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	t.Parallel()
	want := task.Cursor{CreatedAt: time.Date(2026, 9, 5, 12, 0, 0, 123456789, time.UTC), ID: uuid.New()}

	got, err := task.DecodeCursor(want.Encode())
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) || got.ID != want.ID {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
	var zero task.Cursor
	if zero.Encode() != "" {
		t.Error("the zero cursor must encode to an empty token")
	}
}

// The store hands out copies. Without that, a caller mutating a returned task
// silently edits the database.
func TestRepositoryReturnsCopies(t *testing.T) {
	t.Parallel()
	svc, _, _ := newService(t)
	created := mustCreate(t, svc, "original", task.StatusIncomplete)

	created.Name = "mutated by the caller"

	got, err := svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "original" {
		t.Errorf("stored name = %q, want %q — the repository handed out a live pointer", got.Name, "original")
	}
}
