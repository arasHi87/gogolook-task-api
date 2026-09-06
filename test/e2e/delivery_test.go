package e2e

import (
	"fmt"
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/task/pgrepo"
	"github.com/arasHi87/gogolook-task-api/test/harness"
)

// TestEventsCrossTheProcessBoundary is the whole system in one scenario.
//
// An HTTP request lands on one process, which writes a task and its event in a
// single transaction and answers. A different process, sharing nothing but the
// database, claims that event and delivers it. Nothing in the api process
// knows the worker exists, and nothing in the worker knows a request happened.
func TestEventsCrossTheProcessBoundary(t *testing.T) {
	t.Parallel()

	sys := harness.Start(t)

	created := sys.API.Create("cross the boundary", 0)
	first := sys.Webhook.AwaitEvent(created.ID, pgrepo.EventCreated)

	switch {
	case first.Event.Name != "cross the boundary":
		t.Errorf("created event name %q", first.Event.Name)
	case first.Event.Version != 1:
		t.Errorf("created event version %d, want 1", first.Event.Version)
	}

	// The two headers answer different questions, and the receiver needs both:
	// the event id says "you have seen this change", the idempotency key says
	// "you have seen this exact delivery attempt".
	if want := first.Event.ID(); first.EventID != want {
		t.Errorf("X-Event-Id %q, want %q", first.EventID, want)
	}
	if first.IdempotencyKey == "" {
		t.Error("no Idempotency-Key; the receiver has nothing to deduplicate a retry on")
	}

	sys.API.Update(created.ID, "crossed", 1)
	updated := sys.Webhook.AwaitEvent(created.ID, pgrepo.EventUpdated)
	if updated.Event.Version != 2 {
		t.Errorf("updated event version %d, want 2", updated.Event.Version)
	}

	sys.API.Delete(created.ID)
	deleted := sys.Webhook.AwaitEvent(created.ID, pgrepo.EventDeleted)

	// 2, not 3: a delete removes the row at the version it had. There is no
	// version 3 of a task that no longer exists, and inventing one would put a
	// number in the event that no row ever carried.
	if deleted.Event.Version != 2 {
		t.Errorf("deleted event version %d, want the version the row had (2)", deleted.Event.Version)
	}
	// The row is gone but its event still carried the name, because the event
	// is a snapshot of the change rather than a pointer at a row that may no
	// longer exist by the time anyone reads it.
	if deleted.Event.Name != "crossed" {
		t.Errorf("deleted event name %q, want the name at deletion", deleted.Event.Name)
	}

	// Three changes, three ids. The update and the delete describe different
	// changes at the same version, and the only thing keeping their ids apart
	// is the event name — without it a receiver told to deduplicate on
	// X-Event-Id drops every deletion, which is what this suite found.
	sys.Webhook.Distinct(3)

	sys.Jobs.AwaitSettled(3).AllSucceeded()
	sys.Tasks.Count(0)
}

// TestEveryAcceptedWriteLeavesExactlyOneJob checks the outbox from outside.
//
// The invariant is not "a job exists somewhere". It is that a write which the
// client was told succeeded left exactly one event, in the same transaction,
// with a key that identifies the change — because the alternative failure is
// silent: an event that was never written, on a request that returned 200.
func TestEveryAcceptedWriteLeavesExactlyOneJob(t *testing.T) {
	t.Parallel()

	// No consumer: the jobs stay where the api process put them, so this
	// observes the write rather than a race with the drain.
	sys := harness.Start(t, harness.WithoutWorker())

	const writes = 5
	ids := make([]string, 0, writes)
	for i := range writes {
		ids = append(ids, sys.API.Create(fmt.Sprintf("outbox %d", i), 0).ID)
	}

	keys := sys.Jobs.All().Count(writes).AllInState("available").UniqueKeys()

	if len(keys) != writes {
		t.Errorf("%d distinct unique keys for %d writes: %v", len(keys), writes, keys)
	}
	for _, id := range ids {
		want := pgrepo.EventCreated + ":" + id + ":1"
		if _, ok := keys[want]; !ok {
			t.Errorf("no job keyed %q; the task was created but its event was not", want)
		}
	}
}
