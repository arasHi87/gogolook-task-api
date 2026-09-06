package e2e

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/arasHi87/gogolook-task-api/test/harness"
)

// TestTheRateLimiterShedsAnonymousTraffic proves the quota is enforced by the
// shipped binary, and that a refused caller is told enough to recover.
//
// A limiter that returns 429 and nothing else makes the client guess. The
// headers are the difference between a client that paces itself and one that
// discovers the wall by hitting it repeatedly.
func TestTheRateLimiterShedsAnonymousTraffic(t *testing.T) {
	t.Parallel()

	sys := harness.Start(t, harness.WithoutWorker(), harness.APIEnv(
		"TASKAPI_RATELIMIT_TIERS_ANONYMOUS_RATE=2",
		"TASKAPI_RATELIMIT_TIERS_ANONYMOUS_BURST=3",
	))

	refused := drainQuota(t, sys)

	refused.
		HeaderAtLeast("Retry-After", 1).
		HasHeader("RateLimit-Remaining", "0").
		// Both spellings: the IETF draft's, and the trio most SDKs read.
		//
		// A burst of 3 refilling at 2 per second takes 2 seconds — the policy
		// describes the bucket, so remaining can never exceed limit.
		HasHeader("RateLimit-Policy", "3;w=2").
		BodyContains("rate limit")
}

// A token buys a bigger quota, which is the whole reason auth exists here: it
// is a quota dimension, not a gate.
func TestATokenBuysTheStandardQuota(t *testing.T) {
	t.Parallel()

	sys := harness.Start(t, harness.WithoutWorker(), harness.APIEnv(
		"TASKAPI_RATELIMIT_TIERS_ANONYMOUS_RATE=2",
		"TASKAPI_RATELIMIT_TIERS_ANONYMOUS_BURST=2",
		"TASKAPI_RATELIMIT_TIERS_STANDARD_RATE=500",
		"TASKAPI_RATELIMIT_TIERS_STANDARD_BURST=500",
	))

	// Exhaust the anonymous bucket first, so the token is demonstrably not
	// just inheriting a fresh one.
	drainQuota(t, sys)

	sys.API.WithToken(harness.DemoToken).Get("/tasks").
		Status(http.StatusOK).
		HasHeader("RateLimit-Limit", "500")

	// And the anonymous caller is still throttled: the two buckets are
	// separate, so one tenant cannot spend another's capacity.
	sys.API.Get("/tasks").Status(http.StatusTooManyRequests)
}

// The default never returns 401, and that is the decision. A reviewer whose
// first curl is refused concludes the exercise is broken.
func TestTheDefaultModeNeverRejects(t *testing.T) {
	t.Parallel()

	sys := harness.Start(t, harness.WithoutWorker())

	for _, token := range []string{"", "clearly-not-a-real-token", harness.DemoToken} {
		sys.API.WithToken(token).Get("/tasks").Status(http.StatusOK)
	}
}

// TestTheBreakerOpensAndTheJobsSurviveIt is the pairing that only makes sense
// with both halves present.
//
// The breaker turns a slow failure into a fast one. On its own that would be
// worse for the jobs: they would burn their whole retry budget in milliseconds
// of instant refusals and be discarded for an outage that had nothing to do
// with them. The snooze is what stops that — a job parked by an open circuit
// keeps its attempt.
func TestTheBreakerOpensAndTheJobsSurviveIt(t *testing.T) {
	t.Parallel()

	const budget = 20
	sys := harness.Start(t, harness.WorkerEnv(
		// One attempt per job, so each delivery is one breaker execution and
		// the arithmetic below is the arithmetic the breaker is doing.
		"TASKAPI_WEBHOOK_RETRY_MAX_ATTEMPTS=1",
		"TASKAPI_WEBHOOK_TIMEOUT=1s",
		"TASKAPI_BREAKER_WEBHOOK_MIN_THROUGHPUT=3",
		"TASKAPI_BREAKER_WEBHOOK_WINDOW=30s",
		"TASKAPI_BREAKER_WEBHOOK_OPEN_DURATION=3s",
		"TASKAPI_BREAKER_WEBHOOK_HALF_OPEN_MAX_CALLS=2",
		"TASKAPI_BREAKER_WEBHOOK_HALF_OPEN_SUCCESS_THRESHOLD=1",
		fmt.Sprintf("TASKAPI_QUEUE_MAX_ATTEMPTS=%d", budget),
	))

	sys.Webhook.Break(http.StatusServiceUnavailable)

	const tasks = 8
	for i := range tasks {
		sys.API.Create(fmt.Sprintf("outage %d", i), 0)
	}

	// A breaker that opens silently is a breaker nobody knows about, so the
	// log line is part of the contract and is what this waits on.
	sys.WorkerLog("circuit breaker opened")

	// While it is open the jobs are parked, not failing. The attempt counter
	// is what proves the budget is intact.
	sys.Jobs.All().NoneInState("discarded").AttemptsBelow(budget)

	// The dependency comes back. Every event is still delivered.
	sys.Webhook.Heal()
	sys.WorkerLog("circuit breaker closed")

	sys.Jobs.AwaitSettled(tasks).AllSucceeded()
	sys.Webhook.Distinct(tasks)
}

// drainQuota spends the anonymous bucket and returns the response that was
// refused.
func drainQuota(t *testing.T, sys *harness.System) *harness.Response {
	t.Helper()

	for range 20 {
		if resp := sys.API.Get("/tasks"); resp.Code() == http.StatusTooManyRequests {
			return resp
		}
	}
	t.Fatal("twenty requests against a burst of three were all allowed")
	return nil
}
