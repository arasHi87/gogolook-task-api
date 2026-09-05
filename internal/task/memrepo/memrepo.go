// Package memrepo is the in-memory task repository.
//
// It is the default backend, and it is what makes `go run ./cmd/taskapi all`
// work on a plane: the whole API, with no Postgres, no Docker and no
// configuration. The specification asked for "any in-memory mechanism"; this is
// that, and everything Postgres adds — durability, the transactional outbox,
// the queue — is opt-in on top.
//
// Archetype: Store. Passive shared state behind an explicit lock, no goroutines
// of its own, no I/O.
package memrepo

import (
	"context"
	"slices"
	"sync"

	"github.com/google/uuid"

	"github.com/arasHi87/gogolook-task-api/internal/task"
)

// Store holds tasks in a map guarded by one mutex.
//
// Reads sort on demand rather than maintaining an ordered index. That is
// O(n log n) per list, which is the wrong shape at scale and exactly right
// here: this backend exists to be obviously correct and dependency-free, and
// the version that has to be fast is the one backed by an index in Postgres.
type Store struct {
	mu    sync.RWMutex
	tasks map[uuid.UUID]*task.Task
}

// New returns an empty store.
func New() *Store {
	return &Store{tasks: make(map[uuid.UUID]*task.Task)}
}

var _ task.Repository = (*Store)(nil)

// Create stores a task. The id is assigned by the service, so a collision here
// means something is badly wrong and is worth failing loudly on rather than
// overwriting.
func (s *Store) Create(ctx context.Context, t *task.Task) (*task.Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.tasks[t.ID]; exists {
		return nil, task.ErrConflict
	}
	// Store a copy: the caller keeps its own pointer, and it must not be a
	// second reference to what is now shared state.
	s.tasks[t.ID] = t.Clone()
	return t.Clone(), nil
}

// Get returns one task.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (*task.Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	t, ok := s.tasks[id]
	if !ok {
		return nil, task.ErrNotFound
	}
	return t.Clone(), nil
}

// Update replaces a task, optionally conditional on its version.
func (s *Store) Update(ctx context.Context, t *task.Task, expectedVersion int64) (*task.Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.tasks[t.ID]
	if !ok {
		return nil, task.ErrNotFound
	}
	if expectedVersion != 0 && existing.Version != expectedVersion {
		return nil, task.ErrConflict
	}

	// created_at is immutable; version is the store's to bump, not the
	// caller's to supply.
	next := t.Clone()
	next.CreatedAt = existing.CreatedAt
	next.Version = existing.Version + 1

	s.tasks[t.ID] = next
	return next.Clone(), nil
}

// Delete removes a task.
func (s *Store) Delete(ctx context.Context, id uuid.UUID) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tasks[id]; !ok {
		return task.ErrNotFound
	}
	delete(s.tasks, id)
	return nil
}

// List returns a page of tasks, newest first.
func (s *Store) List(ctx context.Context, q task.ListQuery) (task.Page, error) {
	if err := ctx.Err(); err != nil {
		return task.Page{}, err
	}

	s.mu.RLock()
	matched := make([]*task.Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		if q.Status != nil && t.Status != *q.Status {
			continue
		}
		if !q.Cursor.After(t) {
			continue
		}
		matched = append(matched, t.Clone())
	}
	s.mu.RUnlock()

	slices.SortFunc(matched, func(a, b *task.Task) int {
		switch {
		case task.Less(a, b):
			return -1
		case task.Less(b, a):
			return 1
		default:
			return 0
		}
	})

	// Fetch one past the page to learn whether another page exists, without a
	// second count query. The extra row is dropped, not returned.
	page := task.Page{Tasks: matched}
	if len(matched) > q.Limit {
		page.Tasks = matched[:q.Limit]
		page.Next = task.CursorOf(page.Tasks[len(page.Tasks)-1])
	}
	return page, nil
}

// Len reports how many tasks are stored. Test and demo affordance only.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.tasks)
}
