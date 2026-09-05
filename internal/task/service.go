package task

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Service is the domain's entry point. Every write goes through it, so the
// rules about names, statuses and versions hold no matter which transport (or
// test, or future CLI) is calling.
type Service struct {
	repo  Repository
	now   func() time.Time
	newID func() uuid.UUID
}

// Option customises a Service. The clock and the id source are injectable so
// tests can assert on ordering and identity without sleeping or guessing.
type Option func(*Service)

// WithClock replaces time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// WithIDSource replaces the identifier generator.
func WithIDSource(next func() uuid.UUID) Option {
	return func(s *Service) {
		if next != nil {
			s.newID = next
		}
	}
}

// NewService wires the domain over a repository.
func NewService(repo Repository, opts ...Option) *Service {
	s := &Service{
		repo:  repo,
		now:   time.Now,
		newID: uuid.New,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// CreateInput is what a caller may set on create. Everything else — id,
// timestamps, version — is the server's to assign, so it is not in this struct
// and cannot be spoofed by a request body.
type CreateInput struct {
	Name   string
	Status Status
}

// Create stores a new task.
//
// Identity and timestamps are assigned here rather than by the database so that
// the in-memory and Postgres backends produce identical results. A default that
// lives in DDL is a default the memory implementation has to reimplement, and
// the two then drift.
func (s *Service) Create(ctx context.Context, in CreateInput) (*Task, error) {
	if err := ValidateName(in.Name); err != nil {
		return nil, err
	}
	if err := ValidateStatus(in.Status); err != nil {
		return nil, err
	}

	now := s.now().UTC()
	t := &Task{
		ID:        s.newID(),
		Name:      in.Name,
		Status:    in.Status,
		CreatedAt: now,
		UpdatedAt: now,
		Version:   1,
	}

	created, err := s.repo.Create(ctx, t)
	if err != nil {
		return nil, fmt.Errorf("create task: %w", err)
	}
	return created, nil
}

// UpdateInput is a full replacement, matching PUT semantics: name and status
// are both required, and omitting one is not a request to leave it alone.
type UpdateInput struct {
	ID     uuid.UUID
	Name   string
	Status Status
	// ExpectedVersion, when non-zero, makes the write conditional. Zero means
	// last-write-wins, which is what a client that never read the task wants.
	ExpectedVersion int64
}

// Update replaces a task.
func (s *Service) Update(ctx context.Context, in UpdateInput) (*Task, error) {
	if in.ID == uuid.Nil {
		return nil, invalid("id", "must not be empty")
	}
	if err := ValidateName(in.Name); err != nil {
		return nil, err
	}
	if err := ValidateStatus(in.Status); err != nil {
		return nil, err
	}
	if in.ExpectedVersion < 0 {
		return nil, invalid("version", "must not be negative")
	}

	updated, err := s.repo.Update(ctx, &Task{
		ID:        in.ID,
		Name:      in.Name,
		Status:    in.Status,
		UpdatedAt: s.now().UTC(),
	}, in.ExpectedVersion)
	if err != nil {
		return nil, fmt.Errorf("update task %s: %w", in.ID, err)
	}
	return updated, nil
}

// Get returns one task.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (*Task, error) {
	if id == uuid.Nil {
		return nil, invalid("id", "must not be empty")
	}
	t, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get task %s: %w", id, err)
	}
	return t, nil
}

// Delete removes a task. Deleting an id that is not there is ErrNotFound rather
// than a silent success: the specification is quiet on the point, and telling
// the caller their id was wrong is more useful than pretending it worked.
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	if id == uuid.Nil {
		return invalid("id", "must not be empty")
	}
	if err := s.repo.Delete(ctx, id); err != nil {
		return fmt.Errorf("delete task %s: %w", id, err)
	}
	return nil
}

// ListInput is the caller's view of a list request, before policy is applied.
type ListInput struct {
	// Status filters by completion state. Nil means all.
	Status *Status
	// PageSize of 0 means the default; anything above the maximum is refused
	// rather than quietly clamped, so a client's paging maths stays honest.
	PageSize int
	// PageToken is the opaque cursor from a previous page.
	PageToken string
}

// List returns a page of tasks, newest first.
func (s *Service) List(ctx context.Context, in ListInput) (Page, error) {
	if in.Status != nil {
		if err := ValidateStatus(*in.Status); err != nil {
			return Page{}, err
		}
	}
	limit, err := clampLimit(in.PageSize)
	if err != nil {
		return Page{}, err
	}
	cursor, err := DecodeCursor(in.PageToken)
	if err != nil {
		return Page{}, err
	}

	page, err := s.repo.List(ctx, ListQuery{Status: in.Status, Limit: limit, Cursor: cursor})
	if err != nil {
		return Page{}, fmt.Errorf("list tasks: %w", err)
	}
	return page, nil
}
