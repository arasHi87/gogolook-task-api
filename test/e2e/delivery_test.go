package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/task/pgrepo"
)

// TestEventsCrossTheProcessBoundary is the whole system in one test.
//
// An HTTP request lands on one process, which writes a task and its event in a
// single transaction and answers. A different process, sharing nothing but the
// database, claims that event and delivers it. Nothing in the api process
// knows the worker exists, and nothing in the worker knows a request happened.
func TestEventsCrossTheProcessBoundary(t *testing.T) {
	t.Parallel()

	h := start(t)

	created := h.API.create("cross the boundary", 0)
	first := h.Sink.awaitEvent(created.ID, pgrepo.EventCreated, 30*time.Second)

	switch {
	case first.Event.Name != "cross the boundary":
		t.Errorf("created event name %q", first.Event.Name)
	case first.Event.Status != 0:
		t.Errorf("created event status %d, want 0", first.Event.Status)
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

	h.API.update(created.ID, "crossed", 1)
	updated := h.Sink.awaitEvent(created.ID, pgrepo.EventUpdated, 30*time.Second)
	switch {
	case updated.Event.Status != 1:
		t.Errorf("updated event status %d, want 1", updated.Event.Status)
	case updated.Event.Version != 2:
		t.Errorf("updated event version %d, want 2", updated.Event.Version)
	}

	h.API.remove(created.ID)
	deleted := h.Sink.awaitEvent(created.ID, pgrepo.EventDeleted, 30*time.Second)
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

	// Three changes, three ids. A delete removes the row at its current
	// version and invents no new one, so the update and the delete describe
	// different changes at the same version — and the only thing keeping their
	// ids apart is the event name. Without it a receiver told to deduplicate
	// on X-Event-Id drops every deletion, which is what this suite found.
	ids := map[string]struct{}{}
	for _, d := range h.Sink.received() {
		ids[d.EventID] = struct{}{}
	}
	if len(ids) != 3 {
		t.Errorf("%d distinct event ids for 3 changes: %v", len(ids), ids)
	}

	h.requireAllSucceeded(h.awaitSettled(3, 30*time.Second))
	if n := h.taskCount(); n != 0 {
		t.Errorf("%d tasks remain after the delete", n)
	}
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
	h := start(t, withoutWorker())

	const writes = 5
	ids := make([]string, 0, writes)
	for i := range writes {
		ids = append(ids, h.API.create(fmt.Sprintf("outbox %d", i), 0).ID)
	}

	jobs := h.jobs()
	if len(jobs) != writes {
		t.Fatalf("%d jobs for %d writes: %s", len(jobs), writes, describe(jobs))
	}

	keys := map[string]struct{}{}
	for _, j := range jobs {
		if j.State != "available" {
			t.Errorf("job %d is %s with no worker running", j.ID, j.State)
		}
		if j.Kind != pgrepo.JobKind {
			t.Errorf("job %d has kind %q, want %q", j.ID, j.Kind, pgrepo.JobKind)
		}
		if j.UniqueKey == nil {
			t.Errorf("job %d has no unique key, so a retried write would enqueue it twice", j.ID)
			continue
		}
		keys[*j.UniqueKey] = struct{}{}
	}
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
