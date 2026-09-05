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

// ID identifies the change this event describes: what happened, to which task,
// at which version.
//
// One string doing two jobs, and they have to agree. The outbox stores it as
// unique_key, so the same change enqueued twice is one row; the delivery sends
// it as X-Event-Id, so a receiver deduplicating on it drops exactly the
// duplicates at-least-once produces.
//
// The event name is part of it, and that is not decoration. A delete carries
// the version of the row it removed, which is the same version the update
// before it carried — so an id of task-and-version alone makes the delete look
// like a replay of the update, and a receiver doing what we told it to do
// silently drops the deletion. The end-to-end suite caught exactly that.
//
// The version is part of it for the opposite reason: two updates are two
// changes and must both be delivered, while a PUT retried after a timeout is
// one change and must not be.
func (e Event) ID() string {
	return e.Event + ":" + e.TaskID.String() + ":" + itoa(e.Version)
}
