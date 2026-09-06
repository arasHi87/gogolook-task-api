package e2e

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/task/pgrepo"
	"github.com/arasHi87/gogolook-task-api/test/harness"
)

// TestCrashedWorkerLosesNoWork kills a worker in the middle of its work.
//
// SIGKILL, not SIGTERM: a crash gets no chance to release a lease, log a
// failure or roll anything back, which is exactly the case a lease exists for.
// The rows are left claimed by a process that no longer exists, and the only
// thing that can free them is the next worker's reaper noticing the lease has
// lapsed.
//
// The webhook is delivered twice, and that is the correct answer rather than a
// tolerated flaw. The killed worker's HTTP call reached the receiver; it died
// before it could record that. There is no transaction spanning our database
// and someone else's HTTP endpoint, so the honest guarantee is at-least-once
// plus an X-Event-Id the receiver deduplicates on.
func TestCrashedWorkerLosesNoWork(t *testing.T) {
	t.Parallel()

	sys := harness.Start(t)

	// Park every delivery inside the receiver, so the kill lands while the
	// jobs are genuinely in flight rather than whenever the timing works out.
	sys.Webhook.Hold()

	const tasks = 3
	for i := range tasks {
		sys.API.Create(fmt.Sprintf("in flight %d", i), 0)
	}
	sys.Webhook.AwaitCount(tasks)

	sys.CrashWorker()

	// Nothing is running any more, but the database still says otherwise: the
	// rows are held by a worker id that no longer exists anywhere.
	sys.Jobs.All().AllInState("running").AllClaimed()

	sys.Webhook.Release()
	sys.StartWorker()

	// Waiting for the reaper's own line rather than for the clock is what
	// keeps this honest about which mechanism recovered the work.
	sys.WorkerLog("expired leases reclaimed")

	sys.Jobs.AwaitSettled(tasks).
		AllSucceeded().
		// The job's own record of what happened to it. "It failed and then
		// worked" with no idea which worker or why is not an answer anyone
		// can act on.
		EachRetried()

	sys.Webhook.Distinct(tasks)
	sys.Webhook.DeliveredMoreThan(tasks)
}

// TestGracefulDrainFinishesInFlightWork is the same crash, done politely.
//
// SIGTERM is what an orchestrator sends before it takes a container away, and
// the difference from the scenario above is the whole point of having a drain:
// no lease expires, no reaper is involved, no event is delivered twice. The
// work in flight finishes and the process exits on its own.
func TestGracefulDrainFinishesInFlightWork(t *testing.T) {
	t.Parallel()

	sys := harness.Start(t)
	sys.Webhook.Hold()

	const tasks = 3
	for i := range tasks {
		sys.API.Create(fmt.Sprintf("draining %d", i), 0)
	}
	sys.Webhook.AwaitCount(tasks)

	sys.DrainWorker()

	sys.Jobs.AwaitSettled(tasks).AllSucceeded()
	sys.Webhook.Delivered(tasks)
}

// TestWebhookOutageRetriesThenRecovers takes the dependency away and gives it
// back.
//
// A 5xx from the receiver is their problem, not ours: the job must be retried
// with backoff and must survive the outage. Attempts are raised well above the
// default so the outage cannot outlast the retry budget and turn this into a
// test of the discard path, which is the scenario below.
func TestWebhookOutageRetriesThenRecovers(t *testing.T) {
	t.Parallel()

	sys := harness.Start(t, harness.WorkerEnv(
		"TASKAPI_QUEUE_MAX_ATTEMPTS=50",
		"TASKAPI_QUEUE_BACKOFF_BASE=200ms",
		"TASKAPI_QUEUE_BACKOFF_MAX=400ms",
	))

	sys.Webhook.Break(http.StatusServiceUnavailable)
	created := sys.API.Create("survive the outage", 0)

	sys.Jobs.AwaitAttempt(2)
	sys.Webhook.Heal()

	sys.Webhook.AwaitEvent(created.ID, pgrepo.EventCreated)
	settled := sys.Jobs.AwaitSettled(1).AllSucceeded().ErrorsMention("503")

	if j := settled.One(); j.Attempt < 2 {
		t.Errorf("job %d succeeded on attempt %d; the outage should have cost it at least one",
			j.ID, j.Attempt)
	}
}

// TestRejectedWebhookIsNotRetried is the other half of that judgement.
//
// A 4xx is our payload being wrong, and it will still be wrong on the fifth
// attempt. Retrying it only delays finding out, and counting our own bug as
// the receiver's outage is how a validation error takes down a healthy
// dependency. The job stops on the first attempt, in a terminal state, having
// been delivered exactly once.
func TestRejectedWebhookIsNotRetried(t *testing.T) {
	t.Parallel()

	sys := harness.Start(t)

	sys.Webhook.Break(http.StatusBadRequest)
	sys.API.Create("rejected on arrival", 0)

	j := sys.Jobs.AwaitSettled(1).AllInState("cancelled").One()
	if j.Attempt != 1 {
		t.Errorf("job %d made %d attempts; a 4xx must not be retried", j.ID, j.Attempt)
	}
	sys.Webhook.Delivered(1)
}
