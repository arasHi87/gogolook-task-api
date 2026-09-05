package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/task/pgrepo"
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
// plus an X-Event-Id the receiver deduplicates on — which is what the test
// asserts, instead of claiming an "exactly once" the design cannot deliver.
func TestCrashedWorkerLosesNoWork(t *testing.T) {
	t.Parallel()

	h := start(t)

	// Park every delivery inside the receiver, so the kill lands while the
	// jobs are genuinely in flight rather than whenever the timing works out.
	h.Sink.hold()

	const tasks = 3
	for i := range tasks {
		h.API.create(fmt.Sprintf("in flight %d", i), 0)
	}
	h.Sink.awaitCount(tasks, 30*time.Second)

	h.worker.kill()

	// Nothing is running any more, but the database still says otherwise: the
	// rows are held by a worker id that no longer exists anywhere.
	for _, j := range h.jobs() {
		if j.State != "running" || j.LockedBy == nil {
			t.Errorf("job %d is %s (locked_by=%v) after the crash; want running and still claimed",
				j.ID, j.State, j.LockedBy)
		}
	}

	h.Sink.release()
	h.restartWorker()

	// "expired leases reclaimed" is the reaper saying it found them. Waiting
	// for the log rather than for the clock is what keeps this test honest
	// about which mechanism recovered the work.
	h.worker.await(func(r record) bool {
		return strings.Contains(r.str("msg"), "expired leases reclaimed")
	}, 60*time.Second, "the reaper to reclaim the crashed worker's leases")

	settled := h.awaitSettled(tasks, 60*time.Second)
	h.requireAllSucceeded(settled)

	if got := h.Sink.distinct(); got != tasks {
		t.Errorf("%d distinct events, want %d: a change was lost", got, tasks)
	}
	if got := len(h.Sink.received()); got <= tasks {
		t.Errorf("%d deliveries for %d events; the pre-crash deliveries are missing, "+
			"so this ran without reproducing the duplicate it exists to demonstrate", got, tasks)
	}

	// The job's own record of what happened to it. "It failed and then worked"
	// with no idea which worker or why is not an answer anyone can act on.
	for _, j := range settled {
		if j.Attempt < 2 {
			t.Errorf("job %d succeeded on attempt %d; it should have been retried after the crash", j.ID, j.Attempt)
		}
		if len(j.AttemptedBy) < 2 {
			t.Errorf("job %d records %d workers; the crashed one and its replacement are two",
				j.ID, len(j.AttemptedBy))
		}
	}
}

// TestGracefulDrainFinishesInFlightWork is the same crash, done politely.
//
// SIGTERM is what an orchestrator sends before it takes a container away, and
// the difference from the test above is the whole point of having a drain: no
// lease expires, no reaper is involved, no event is delivered twice. The work
// in flight finishes and the process then exits on its own.
func TestGracefulDrainFinishesInFlightWork(t *testing.T) {
	t.Parallel()

	h := start(t)
	h.Sink.hold()

	const tasks = 3
	for i := range tasks {
		h.API.create(fmt.Sprintf("draining %d", i), 0)
	}
	h.Sink.awaitCount(tasks, 30*time.Second)

	// Signal first, then let the receiver answer: the drain has to be already
	// under way while the handlers are still running, or this proves nothing.
	h.worker.signal(syscall.SIGTERM)
	h.Sink.release()

	if err := h.worker.stop(30 * time.Second); err != nil {
		t.Errorf("worker drain: %v", err)
	}

	h.requireAllSucceeded(h.awaitSettled(tasks, 30*time.Second))

	if got := len(h.Sink.received()); got != tasks {
		t.Errorf("%d deliveries for %d events; a graceful drain should not have retried anything: %s",
			got, tasks, summarise(h.Sink.received()))
	}
}

// TestWebhookOutageRetriesThenRecovers takes the dependency away and gives it
// back.
//
// A 5xx from the receiver is their problem, not ours: the job must be retried
// with backoff and must survive the outage. Attempts are raised well above the
// default so the outage cannot outlast the retry budget and turn this into a
// test of the discard path, which is the test below.
func TestWebhookOutageRetriesThenRecovers(t *testing.T) {
	t.Parallel()

	h := start(t, withWorkerEnv(
		"TASKAPI_QUEUE_MAX_ATTEMPTS=50",
		"TASKAPI_QUEUE_BACKOFF_BASE=200ms",
		"TASKAPI_QUEUE_BACKOFF_MAX=400ms",
	))

	h.Sink.fail(503)
	created := h.API.create("survive the outage", 0)

	h.awaitJobs(func(js []job) bool {
		return len(js) == 1 && js[0].Attempt >= 2
	}, 30*time.Second, "the job to be retried at least once")

	h.Sink.heal()

	h.Sink.awaitEvent(created.ID, pgrepo.EventCreated, 60*time.Second)
	settled := h.awaitSettled(1, 30*time.Second)
	h.requireAllSucceeded(settled)

	j := settled[0]
	if j.Attempt < 2 {
		t.Errorf("job %d succeeded on attempt %d; the outage should have cost it at least one", j.ID, j.Attempt)
	}
	if len(j.Errors) == 0 {
		t.Error("the job kept no record of the failures it survived")
	}
	if !strings.Contains(errorText(j), "503") {
		t.Errorf("the recorded errors do not mention the 503 that caused them: %s", errorText(j))
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

	h := start(t)

	h.Sink.fail(400)
	h.API.create("rejected on arrival", 0)

	settled := h.awaitSettled(1, 30*time.Second)
	j := settled[0]

	if j.State != "cancelled" {
		t.Errorf("job %d is %s, want cancelled: a rejected payload is terminal, not an outage", j.ID, j.State)
	}
	if j.Attempt != 1 {
		t.Errorf("job %d made %d attempts; a 4xx must not be retried", j.ID, j.Attempt)
	}
	if got := len(h.Sink.received()); got != 1 {
		t.Errorf("%d deliveries; the receiver was called again after rejecting the event", got)
	}
}

// errorText flattens a job's recorded errors for a failure message.
func errorText(j job) string {
	out, err := json.Marshal(j.Errors)
	if err != nil {
		return fmt.Sprintf("%v", j.Errors)
	}
	return string(out)
}
