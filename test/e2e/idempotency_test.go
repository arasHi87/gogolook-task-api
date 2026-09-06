package e2e

import (
	"net/http"
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/idempotency"
	"github.com/arasHi87/gogolook-task-api/internal/task/pgrepo"
	"github.com/arasHi87/gogolook-task-api/test/harness"
)

const createBody = `{"name":"retried","status":0}`

// TestARetriedWriteExecutesOnce is the producer half of at-least-once, proved
// where it matters: over a socket, against the shipped binary, on the durable
// store.
//
// The unit tests show the middleware replays. This shows what replaying is
// actually for — one task row, one event, one webhook — because a middleware
// that returned the right bytes while the write ran twice would pass those and
// fail here.
func TestARetriedWriteExecutesOnce(t *testing.T) {
	t.Parallel()

	sys := harness.Start(t)
	api := sys.API.WithKey("9f1c2f8a-0b6d-4a1e-9c3f-2b7d5e8a4c11")

	first := api.Post("/tasks", createBody).Status(http.StatusOK).NoHeader(idempotency.HeaderReplayed)

	// The same request again, exactly as a client whose connection dropped
	// before the response arrived would send it.
	api.Post("/tasks", createBody).
		// Byte-identical. The stored response is bytea rather than jsonb for
		// exactly this: a re-rendered body is a different body to a client
		// that compares, hashes or measures it.
		SameBodyAs(first).
		// The header is how a client verifies its own retry logic. Without it
		// a replay is indistinguishable from a second execution.
		HasHeader(idempotency.HeaderReplayed, "true")

	// One task, one event, one delivery — which is the claim. Two of anything
	// here means the replay returned the right bytes while the write happened
	// twice, which is the failure the header exists to prevent.
	sys.Tasks.Count(1)

	created := sys.Webhook.AwaitEvent(first.Task().ID, pgrepo.EventCreated)
	if created.Event.Version != 1 {
		t.Errorf("event version %d, want 1", created.Event.Version)
	}

	sys.Jobs.AwaitSettled(1).AllSucceeded()
	sys.Webhook.Delivered(1)
}

// The same key with a different request is the client's bug, and answering it
// with the first request's response would be ours.
func TestKeyReuseIsRejectedEndToEnd(t *testing.T) {
	t.Parallel()

	sys := harness.Start(t, harness.WithoutWorker())
	api := sys.API.WithKey("reused-key-0001")

	api.Post("/tasks", `{"name":"first","status":0}`).Status(http.StatusOK)
	api.Post("/tasks", `{"name":"second","status":0}`).Status(http.StatusUnprocessableEntity)

	sys.Tasks.Count(1)
}

// The two surfaces are one endpoint served twice, so a key used on one and
// retried on the other is a retry. Telling that client 422 would be refusing
// it for following the contract we published.
func TestAKeyWorksAcrossBothSurfaces(t *testing.T) {
	t.Parallel()

	sys := harness.Start(t, harness.WithoutWorker())
	const key = "cross-surface-0001"
	const body = `{"name":"either surface","status":0}`

	first := sys.API.At("/api/v1").WithKey(key).Post("/tasks", body).Status(http.StatusOK)

	sys.API.At("").WithKey(key).Post("/tasks", body).
		Status(http.StatusOK).
		SameBodyAs(first).
		HasHeader(idempotency.HeaderReplayed, "true")

	sys.Tasks.Count(1)
}

// A key that produced nothing must not be remembered: the client would then be
// refused a retry of a request that never happened.
func TestAFailedWriteDoesNotConsumeItsKey(t *testing.T) {
	t.Parallel()

	sys := harness.Start(t, harness.WithoutWorker())
	api := sys.API.WithKey("released-on-failure")

	// status 2 is not a task that is twice as done.
	api.Post("/tasks", `{"name":"bad","status":2}`).Status(http.StatusBadRequest)

	api.Post("/tasks", `{"name":"good","status":0}`).
		Status(http.StatusOK).
		NoHeader(idempotency.HeaderReplayed)

	sys.Tasks.Count(1)
	// One key stored, not two: the failed attempt left nothing behind.
	sys.Keys.Count(1)
}
