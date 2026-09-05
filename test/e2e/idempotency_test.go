package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/idempotency"
	"github.com/arasHi87/gogolook-task-api/internal/task/pgrepo"
)

// TestARetriedWriteExecutesOnce is the producer half of at-least-once, proved
// where it matters: over a socket, against the shipped binary, on the durable
// store.
//
// The unit tests show the middleware replays. This shows what replaying is
// actually for — one task row, one event, one webhook — because a middleware
// that returns the right bytes while the write ran twice would pass those and
// fail here.
func TestARetriedWriteExecutesOnce(t *testing.T) {
	t.Parallel()

	h := start(t)
	const key = "9f1c2f8a-0b6d-4a1e-9c3f-2b7d5e8a4c11"

	first, firstBody := h.API.keyed(key, http.MethodPost, "/tasks", `{"name":"retried","status":0}`)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first write: status %d: %s", first.StatusCode, firstBody)
	}

	// The same request again, exactly as a client whose connection dropped
	// before the response arrived would send it.
	second, secondBody := h.API.keyed(key, http.MethodPost, "/tasks", `{"name":"retried","status":0}`)

	switch {
	case second.StatusCode != first.StatusCode:
		t.Errorf("replay status %d, want %d", second.StatusCode, first.StatusCode)
	case secondBody != firstBody:
		// Byte-identical. The stored response is bytea rather than jsonb for
		// exactly this: a re-rendered body is a different body to a client
		// that compares, hashes or measures it.
		t.Errorf("replay body\n got %s\nwant %s", secondBody, firstBody)
	case second.Header.Get(idempotency.HeaderReplayed) != "true":
		t.Errorf("%s = %q, want true", idempotency.HeaderReplayed,
			second.Header.Get(idempotency.HeaderReplayed))
	}

	// One task, one event, one delivery — which is the claim. Two of anything
	// here means the replay returned the right bytes while the write happened
	// twice, which is the failure the header exists to prevent.
	if n := h.taskCount(); n != 1 {
		t.Errorf("%d tasks after the retry, want 1", n)
	}

	created := h.Sink.awaitEvent(idOf(t, firstBody), pgrepo.EventCreated, 30*time.Second)
	h.requireAllSucceeded(h.awaitSettled(1, 30*time.Second))

	if got := len(h.Sink.received()); got != 1 {
		t.Errorf("%d webhook deliveries for one logical write: %s", got, summarise(h.Sink.received()))
	}
	if created.Event.Version != 1 {
		t.Errorf("event version %d, want 1", created.Event.Version)
	}
}

// The same key with a different request is the client's bug, and answering it
// with the first request's response would be ours.
func TestKeyReuseIsRejectedEndToEnd(t *testing.T) {
	t.Parallel()

	h := start(t, withoutWorker())
	const key = "reused-key-0001"

	h.API.keyed(key, http.MethodPost, "/tasks", `{"name":"first","status":0}`)
	resp, body := h.API.keyed(key, http.MethodPost, "/tasks", `{"name":"second","status":0}`)

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status %d, want 422: %s", resp.StatusCode, body)
	}
	if n := h.taskCount(); n != 1 {
		t.Errorf("%d tasks; the rejected request must not have executed", n)
	}
}

// The two surfaces are one endpoint served twice, so a key used on one and
// retried on the other is a retry. Telling that client 422 would be refusing
// it for following the contract we published.
func TestAKeyWorksAcrossBothSurfaces(t *testing.T) {
	t.Parallel()

	h := start(t, withoutWorker())
	const key = "cross-surface-0001"
	const body = `{"name":"either surface","status":0}`

	versioned := h.at("/api/v1")
	unversioned := h.at("")

	first, firstBody := versioned.keyed(key, http.MethodPost, "/tasks", body)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first write: status %d: %s", first.StatusCode, firstBody)
	}

	second, secondBody := unversioned.keyed(key, http.MethodPost, "/tasks", body)
	switch {
	case second.StatusCode != http.StatusOK:
		t.Errorf("retry on the unversioned surface: status %d, want 200: %s", second.StatusCode, secondBody)
	case second.Header.Get(idempotency.HeaderReplayed) != "true":
		t.Error("the retry executed again instead of replaying")
	case secondBody != firstBody:
		t.Errorf("replay body\n got %s\nwant %s", secondBody, firstBody)
	}

	if n := h.taskCount(); n != 1 {
		t.Errorf("%d tasks, want 1", n)
	}
}

// A key that produced nothing must not be remembered: the client would then be
// refused a retry of a request that never happened. The rejection here is
// validation, which fails before the domain is reached — the case where the
// key is reserved and everything after it has to roll back.
func TestAFailedWriteDoesNotConsumeItsKey(t *testing.T) {
	t.Parallel()

	h := start(t, withoutWorker())
	const key = "released-on-failure"

	// status 2 is not a task that is twice as done.
	if resp, body := h.API.keyed(key, http.MethodPost, "/tasks", `{"name":"bad","status":2}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid write: status %d, want 400: %s", resp.StatusCode, body)
	}

	resp, body := h.API.keyed(key, http.MethodPost, "/tasks", `{"name":"good","status":0}`)
	switch {
	case resp.StatusCode != http.StatusOK:
		t.Errorf("retry after a rejected write: status %d, want 200: %s", resp.StatusCode, body)
	case resp.Header.Get(idempotency.HeaderReplayed) != "":
		t.Error("the retry was answered as a replay of a response that was never stored")
	}

	if n := h.taskCount(); n != 1 {
		t.Errorf("%d tasks, want 1", n)
	}
	if rows := h.keyRows(); rows != 1 {
		t.Errorf("%d idempotency keys stored, want 1: the failed attempt left a row behind", rows)
	}
}

// idOf pulls the task id out of a create response.
func idOf(t *testing.T, body string) string {
	t.Helper()

	var task taskJSON
	if err := unmarshal(body, &task); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return task.ID
}
