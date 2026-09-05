package pgrepo

import (
	"time"

	"github.com/google/uuid"

	"github.com/arasHi87/gogolook-task-api/internal/task"
)

// JobKind is the queue job every task change produces.
const JobKind = "task.event"

// Event names.
const (
	EventCreated = "task.created"
	EventUpdated = "task.updated"
	EventDeleted = "task.deleted"
)

// Event is the payload of a task.event job.
//
// It carries the task's state at the moment of the change rather than just its
// id, because a handler that re-reads the row sees whatever the row looks like
// when the job runs — which after two more updates is not the event it was
// told about. Delivering a stale-but-correct snapshot beats delivering a
// current-but-wrong one.
type Event struct {
	Event      string    `json:"event"`
	TaskID     uuid.UUID `json:"task_id"`
	Version    int64     `json:"version"`
	Name       string    `json:"name,omitempty"`
	Status     int32     `json:"status"`
	OccurredAt time.Time `json:"occurred_at"`
}

// eventFor builds the payload for a change to t.
func eventFor(name string, t *task.Task) Event {
	return Event{
		Event:      name,
		TaskID:     t.ID,
		Version:    t.Version,
		Name:       t.Name,
		Status:     int32(t.Status),
		OccurredAt: t.UpdatedAt,
	}
}

// uniqueKey deduplicates an event.
//
// The version is part of the key, so two updates produce two events but a
// retry of the same update produces one. Without it, a caller that retries a
// PUT after a timeout would deliver the same change twice.
func uniqueKey(event string, id uuid.UUID, version int64) string {
	return event + ":" + id.String() + ":" + itoa(version)
}
