package e2e

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// demoStandardToken is the plaintext behind the compiled-in hash, published in
// .env.example so the quickstart and this test agree on one credential.
const demoStandardToken = "demo-standard-token"

// TestTheRateLimiterShedsAnonymousTraffic proves the quota is enforced by the
// shipped binary, and that a refused caller is told enough to recover.
//
// A limiter that returns 429 and nothing else makes the client guess. The
// headers are the difference between a client that paces itself and one that
// discovers the wall by hitting it repeatedly.
func TestTheRateLimiterShedsAnonymousTraffic(t *testing.T) {
	t.Parallel()

	h := start(t, withoutWorker(), withAPIEnv(
		"TASKAPI_RATELIMIT_TIERS_ANONYMOUS_RATE=2",
		"TASKAPI_RATELIMIT_TIERS_ANONYMOUS_BURST=3",
	))

	var refused *http.Response
	var body string
	for range 20 {
		resp, raw := h.API.do(http.MethodGet, "/tasks", nil)
		if resp.StatusCode == http.StatusTooManyRequests {
			refused, body = resp, string(raw)
			break
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("unexpected status %d: %s", resp.StatusCode, raw)
		}
	}
	if refused == nil {
		t.Fatal("twenty requests against a burst of three were all allowed")
	}

	if got := refused.Header.Get("Retry-After"); toInt(t, got) < 1 {
		t.Errorf("Retry-After = %q, want at least 1", got)
	}
	if got := refused.Header.Get("RateLimit-Remaining"); got != "0" {
		t.Errorf("RateLimit-Remaining = %q, want 0", got)
	}
	// Both spellings: the IETF draft's, and the trio most SDKs actually read.
	//
	// A burst of 3 refilling at 2 per second takes 2 seconds — the policy
	// describes the bucket, so remaining can never exceed limit.
	if got := refused.Header.Get("RateLimit-Policy"); got != "3;w=2" {
		t.Errorf("RateLimit-Policy = %q, want %q", got, "3;w=2")
	}
	if got := refused.Header.Get("RateLimit"); !strings.Contains(got, "limit=3") {
		t.Errorf("RateLimit = %q, want the tier's limit", got)
	}
	if !strings.Contains(body, "rate limit") {
		t.Errorf("body does not say what happened: %s", body)
	}
}

// A token buys a bigger quota, which is the whole reason auth exists here: it
// is a quota dimension, not a gate.
func TestATokenBuysTheStandardQuota(t *testing.T) {
	t.Parallel()

	h := start(t, withoutWorker(), withAPIEnv(
		"TASKAPI_RATELIMIT_TIERS_ANONYMOUS_RATE=2",
		"TASKAPI_RATELIMIT_TIERS_ANONYMOUS_BURST=2",
		"TASKAPI_RATELIMIT_TIERS_STANDARD_RATE=500",
		"TASKAPI_RATELIMIT_TIERS_STANDARD_BURST=500",
	))

	// Exhaust the anonymous bucket first, so the token is demonstrably not
	// just inheriting a fresh one.
	for range 6 {
		h.API.do(http.MethodGet, "/tasks", nil)
	}

	resp, body := h.API.bearer(demoStandardToken, http.MethodGet, "/tasks")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated request: status %d, want 200: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("RateLimit-Limit"); got != "500" {
		t.Errorf("RateLimit-Limit = %q, want the standard tier's 500", got)
	}

	// And the anonymous caller is still throttled: the two buckets are
	// separate, so one tenant cannot spend another's capacity.
	anon, _ := h.API.do(http.MethodGet, "/tasks", nil)
	if anon.StatusCode != http.StatusTooManyRequests {
		t.Errorf("anonymous status %d, want 429: the token refilled the wrong bucket", anon.StatusCode)
	}
}

// The default never returns 401, and that is the decision. A reviewer whose
// first curl is refused concludes the exercise is broken.
func TestTheDefaultModeNeverRejects(t *testing.T) {
	t.Parallel()

	h := start(t, withoutWorker())

	for _, token := range []string{"", "clearly-not-a-real-token", demoStandardToken} {
		resp, body := h.API.bearer(token, http.MethodGet, "/tasks")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("token %q: status %d, want 200: %s", token, resp.StatusCode, body)
		}
	}
}

// TestTheBreakerOpensAndTheJobsSurviveIt is the pairing that only makes sense
// with both halves present.
//
// The breaker turns a slow failure into a fast one. On its own that is worse
// for the job: it would burn its whole retry budget in milliseconds of instant
// refusals and be discarded for an outage that had nothing to do with it. The
// snooze is what stops that — and the proof is that the attempt counter does
// not climb while the circuit is open.
func TestTheBreakerOpensAndTheJobsSurviveIt(t *testing.T) {
	t.Parallel()

	h := start(t, withWorkerEnv(
		// One attempt per job, so each delivery is one breaker execution and
		// the arithmetic below is the arithmetic the breaker is doing.
		"TASKAPI_WEBHOOK_RETRY_MAX_ATTEMPTS=1",
		"TASKAPI_WEBHOOK_TIMEOUT=1s",
		"TASKAPI_BREAKER_WEBHOOK_MIN_THROUGHPUT=3",
		"TASKAPI_BREAKER_WEBHOOK_WINDOW=30s",
		"TASKAPI_BREAKER_WEBHOOK_OPEN_DURATION=3s",
		"TASKAPI_BREAKER_WEBHOOK_HALF_OPEN_MAX_CALLS=2",
		"TASKAPI_BREAKER_WEBHOOK_HALF_OPEN_SUCCESS_THRESHOLD=1",
		// Retries are cheap so the jobs keep arriving at the breaker.
		"TASKAPI_QUEUE_MAX_ATTEMPTS=20",
	))

	h.Sink.fail(http.StatusServiceUnavailable)

	const tasks = 8
	for i := range tasks {
		h.API.create("outage "+strconv.Itoa(i), 0)
	}

	// A breaker that opens silently is a breaker nobody knows about, so the
	// log line is part of the contract and is what this waits on.
	h.worker.await(func(r record) bool {
		return r.str("msg") == "circuit breaker opened"
	}, 60*time.Second, "the circuit to open")

	// While it is open the jobs are parked, not failing. scheduled is the
	// snooze state; the attempt counter is what proves the budget is intact.
	for _, j := range h.jobs() {
		if j.State == "discarded" {
			t.Errorf("job %d was discarded during the outage; its budget was spent on someone else's failure", j.ID)
		}
		if j.Attempt >= 20 {
			t.Errorf("job %d is on attempt %d; the snooze did not preserve its budget", j.ID, j.Attempt)
		}
	}

	// The dependency comes back. Every event is still delivered.
	h.Sink.heal()
	h.worker.await(func(r record) bool {
		return r.str("msg") == "circuit breaker closed"
	}, 60*time.Second, "the circuit to close")

	h.requireAllSucceeded(h.awaitSettled(tasks, 60*time.Second))
	if got := h.Sink.distinct(); got != tasks {
		t.Errorf("%d distinct events delivered, want %d: the outage lost a change", got, tasks)
	}
}

func toInt(t *testing.T, s string) int {
	t.Helper()

	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("expected an integer, got %q", s)
	}
	return n
}
