// Package task is the domain.
//
// Nothing here knows about HTTP, Connect or Protobuf: Service takes and returns
// plain Go types and plain Go errors. That is what makes internal/api a thin,
// replaceable adapter — if the transcoding layer ever has to be swapped for a
// hand-written router, the blast radius is one package and this one does not
// move.
package task

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/arasHi87/gogolook-task-api/internal/apperr"
)

// Status is the completion state. The specified contract is an integer with
// exactly two values; this type names them without changing the wire form.
type Status int32

const (
	// StatusIncomplete is 0: the task is not done.
	StatusIncomplete Status = 0
	// StatusCompleted is 1: the task is done.
	StatusCompleted Status = 1
)

// Valid reports whether s is one of the two defined values.
func (s Status) Valid() bool { return s == StatusIncomplete || s == StatusCompleted }

// String renders the status for logs and error messages.
func (s Status) String() string {
	switch s {
	case StatusIncomplete:
		return "incomplete"
	case StatusCompleted:
		return "completed"
	default:
		return fmt.Sprintf("Status(%d)", int32(s))
	}
}

// MaxNameLength bounds the name. Unbounded text in a database column is how a
// request body becomes a storage incident.
const MaxNameLength = 255

// Task is a unit of work.
//
// Name and status are the specified fields. The rest is the housekeeping every
// real table has: identity, when it changed, and a version for detecting
// concurrent edits.
type Task struct {
	ID        uuid.UUID
	Name      string
	Status    Status
	CreatedAt time.Time
	UpdatedAt time.Time
	// Version starts at 1 and increments on every update. A caller that sends
	// it back on update is told about a concurrent edit instead of silently
	// overwriting it.
	Version int64
}

// Clone returns a copy. Repositories hand out clones so a caller cannot reach
// back into stored state, which is the whole difference between an in-memory
// repository and a shared mutable map.
func (t *Task) Clone() *Task {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

// Sentinel errors.
//
// They carry an apperr.Kind, so a transport maps them from one constant-size
// table rather than from a case per package, and errors.Is still reads the way
// it always has. Nothing here names a status code: the same error has to be
// usable from the HTTP handler, from the queue worker, and from a test with no
// transport at all.
var (
	// ErrNotFound is returned when no task has the requested id.
	ErrNotFound = apperr.New(apperr.NotFound, "task not found")

	// ErrInvalidArgument is returned when the caller's input is unusable.
	// Field-scoped problems below match it, because they are the same kind.
	ErrInvalidArgument = apperr.New(apperr.Invalid, "invalid argument")

	// ErrConflict is returned when an update's expected version does not match
	// what is stored: someone else changed the task first.
	ErrConflict = apperr.New(apperr.Conflict, "the task was modified by someone else")
)

// invalid names the offending input. The field reaches the client, because it
// is the client's own input being described back to them.
func invalid(field, format string, args ...any) error {
	return apperr.Field(field, format, args...)
}

// ValidateName applies the name rule shared by create and update.
//
// The same constraint is declared in the proto for protovalidate to enforce at
// the edge. It is repeated here on purpose: the domain must be correct when
// called directly — from a test, from the queue, from a future CLI — and not
// only when something upstream happened to check first.
func ValidateName(name string) error {
	trimmed := strings.TrimSpace(name)
	switch {
	case trimmed == "":
		return invalid("name", "must not be empty")
	case len(name) > MaxNameLength:
		return invalid("name", "must be at most %d characters, got %d", MaxNameLength, len(name))
	}
	return nil
}

// ValidateStatus applies the status rule.
func ValidateStatus(s Status) error {
	if !s.Valid() {
		return invalid("status", "must be 0 (incomplete) or 1 (completed), got %d", int32(s))
	}
	return nil
}
