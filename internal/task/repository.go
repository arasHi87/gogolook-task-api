package task

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Repository is the storage port.
//
// It is declared here, where it is used, with the minimal method set — and it
// exists at all only because a second implementation genuinely does: the
// in-memory store that satisfies the exercise with no dependencies, and the
// Postgres one that carries the transactional outbox.
type Repository interface {
	// Create stores a new task. The task is fully formed: the service assigns
	// its id, timestamps and initial version.
	Create(ctx context.Context, t *Task) (*Task, error)

	// Get returns one task, or ErrNotFound.
	Get(ctx context.Context, id uuid.UUID) (*Task, error)

	// Update replaces a task. When expectedVersion is non-zero the write is
	// conditional on it and returns ErrConflict if it does not match.
	Update(ctx context.Context, t *Task, expectedVersion int64) (*Task, error)

	// Delete removes a task, or returns ErrNotFound.
	Delete(ctx context.Context, id uuid.UUID) error

	// List returns a page of tasks, newest first.
	List(ctx context.Context, q ListQuery) (Page, error)
}

// Pagination defaults. A caller that asks for nothing gets 20; one that asks
// for a thousand gets 100.
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// ListQuery selects a page of tasks.
type ListQuery struct {
	// Status filters by completion state. Nil means all.
	Status *Status
	// Limit is the page size, already clamped by the service.
	Limit int
	// Cursor is the keyset position from a previous page. Zero means the start.
	Cursor Cursor
}

// Page is one page of results.
type Page struct {
	Tasks []*Task
	// Next is the cursor for the following page, zero when there are no more.
	Next Cursor
}

// Cursor is a keyset position: the (created_at, id) of the last row returned.
//
// Keyset rather than offset, because an offset scan reads and discards every
// row before the page — cost grows with depth — and it silently skips or
// repeats rows when the table changes between requests. A keyset cursor is one
// index seek regardless of depth and is stable under concurrent writes.
type Cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// IsZero reports whether the cursor is the start of the sequence.
func (c Cursor) IsZero() bool { return c.CreatedAt.IsZero() && c.ID == uuid.Nil }

// Encode renders the cursor as an opaque page token.
//
// Opaque is the point: clients must not construct or reason about it, so the
// ordering key can change without breaking them. It is base64 rather than
// encrypted because it contains nothing a client cannot already see.
func (c Cursor) Encode() string {
	if c.IsZero() {
		return ""
	}
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses a page token. An unparseable token is a client error, not
// a silent reset to page one: silently restarting is how a paginating client
// ends up in an infinite loop.
func DecodeCursor(token string) (Cursor, error) {
	if token == "" {
		return Cursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Cursor{}, invalid("page_token", "is not a valid page token")
	}
	at, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return Cursor{}, invalid("page_token", "is not a valid page token")
	}
	ts, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return Cursor{}, invalid("page_token", "is not a valid page token")
	}
	uid, err := uuid.Parse(id)
	if err != nil {
		return Cursor{}, invalid("page_token", "is not a valid page token")
	}
	return Cursor{CreatedAt: ts, ID: uid}, nil
}

// After reports whether t sorts strictly after the cursor position in the list
// ordering (created_at descending, then id descending).
//
// Ties on created_at are broken by id so the ordering is total. Without that
// tiebreak, two tasks created in the same clock tick can straddle a page
// boundary and be returned twice or not at all.
func (c Cursor) After(t *Task) bool {
	if c.IsZero() {
		return true
	}
	switch {
	case t.CreatedAt.After(c.CreatedAt):
		return false
	case t.CreatedAt.Before(c.CreatedAt):
		return true
	default:
		return t.ID.String() < c.ID.String()
	}
}

// CursorOf is the position of t, for building the next page token.
func CursorOf(t *Task) Cursor {
	return Cursor{CreatedAt: t.CreatedAt, ID: t.ID}
}

// Less orders two tasks the way List returns them: newest first, id descending
// as the tiebreak.
func Less(a, b *Task) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.ID.String() > b.ID.String()
}

// clampLimit applies the page-size policy.
func clampLimit(n int) (int, error) {
	switch {
	case n < 0:
		return 0, invalid("page_size", fmt.Sprintf("must not be negative, got %d", n))
	case n == 0:
		return DefaultPageSize, nil
	case n > MaxPageSize:
		return 0, invalid("page_size", fmt.Sprintf("must be at most %d, got %d", MaxPageSize, n))
	default:
		return n, nil
	}
}
