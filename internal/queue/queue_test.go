package queue_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/apperr"
	"github.com/arasHi87/gogolook-task-api/internal/queue"
)

const kind = "test.job"

// The core correctness claim, and the reason FOR UPDATE SKIP LOCKED is there:
// N workers over M jobs must run each job exactly once. Anything less and the
// queue is a way to do work twice.
func TestEveryJobRunsExactlyOnce(t *testing.T) {
	t.Parallel()
	pool := newPostgres(t)

	const jobs = 300
	enqueue(t, pool, kind, jobs)

	var (
		mu   sync.Mutex
		runs = map[int64]int{}
	)
	cfg := testConfig()
	cfg.Workers = 16
	cfg.ClaimBatch = 10

	_, events := runPool(t, pool, cfg, map[string]queue.HandlerFunc{
		kind: func(_ context.Context, j *queue.Job) error {
			mu.Lock()
			runs[j.ID]++
			mu.Unlock()
			return nil
		},
	})

	got := await(t, events, jobs, 60*time.Second)
	for _, e := range got {
		if e.Outcome != queue.OutcomeSucceeded {
			t.Fatalf("job %d: %s (%v)", e.JobID, e.Outcome, e.Err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(runs) != jobs {
		t.Fatalf("%d distinct jobs ran, want %d", len(runs), jobs)
	}
	for id, n := range runs {
		if n != 1 {
			t.Errorf("job %d ran %d times, want once", id, n)
		}
	}
}

// A failure is retried with a backoff, and the error history records every
// attempt rather than only the last.
func TestFailureIsRetriedThenDiscarded(t *testing.T) {
	t.Parallel()
	pool := newPostgres(t)
	enqueue(t, pool, kind, 1)

	cfg := testConfig()
	cfg.MaxAttempts = 3

	var attempts atomic.Int64
	_, events := runPool(t, pool, cfg, map[string]queue.HandlerFunc{
		kind: func(context.Context, *queue.Job) error {
			attempts.Add(1)
			// An unclassified error is Internal, which is retryable: a bug on
			// our side might not repeat.
			return errors.New("dependency exploded")
		},
	})

	discarded := awaitOutcome(t, events, queue.OutcomeDiscarded, 30*time.Second)
	if discarded.Attempt != cfg.MaxAttempts {
		t.Errorf("discarded on attempt %d, want %d", discarded.Attempt, cfg.MaxAttempts)
	}
	if got := attempts.Load(); got != int64(cfg.MaxAttempts) {
		t.Errorf("handler ran %d times, want %d", got, cfg.MaxAttempts)
	}

	row := readJob(t, pool, discarded.JobID)
	if row.State != queue.StateDiscarded {
		t.Errorf("state = %s, want discarded", row.State)
	}
	// "It failed three times" with only the third error is not something
	// anyone can act on.
	if len(row.Errors) != cfg.MaxAttempts {
		t.Errorf("error history has %d entries, want %d: %+v", len(row.Errors), cfg.MaxAttempts, row.Errors)
	}
	if len(row.AttemptedBy) != cfg.MaxAttempts {
		t.Errorf("attempted_by has %d entries, want %d", len(row.AttemptedBy), cfg.MaxAttempts)
	}
}

// The single most valuable thing a handler can say. Retrying a poison job five
// times is pure waste and hides the real bug behind a queue that looks slow.
func TestTerminalFailureIsNotRetried(t *testing.T) {
	t.Parallel()
	pool := newPostgres(t)
	enqueue(t, pool, kind, 1)

	var attempts atomic.Int64
	_, events := runPool(t, pool, testConfig(), map[string]queue.HandlerFunc{
		kind: func(context.Context, *queue.Job) error {
			attempts.Add(1)
			return queue.Terminal(errors.New("payload will never parse"))
		},
	})

	e := awaitOutcome(t, events, queue.OutcomeCancelled, 15*time.Second)
	if e.Attempt != 1 {
		t.Errorf("cancelled on attempt %d, want 1", e.Attempt)
	}
	if got := readJob(t, pool, e.JobID).State; got != queue.StateCancelled {
		t.Errorf("state = %s, want cancelled", got)
	}

	// Nothing should pick it up again.
	time.Sleep(300 * time.Millisecond) //nolint:forbidigo // proving absence needs a window
	if got := attempts.Load(); got != 1 {
		t.Errorf("handler ran %d times, want once", got)
	}
}

// The claim this makes concrete: an error that is a client's mistake over HTTP
// is a poison job in the queue, and the two must not disagree. The
// classification is the same apperr.Kind the transport maps to a status.
func TestClassificationDecidesRetryability(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		err  error
		want queue.Outcome
	}{
		"invalid is poison":       {apperr.New(apperr.Invalid, "bad payload"), queue.OutcomeCancelled},
		"not found is poison":     {apperr.New(apperr.NotFound, "gone"), queue.OutcomeCancelled},
		"conflict is poison":      {apperr.New(apperr.Conflict, "changed"), queue.OutcomeCancelled},
		"unavailable is retried":  {apperr.New(apperr.Unavailable, "sink is down"), queue.OutcomeRetried},
		"timeout is retried":      {apperr.New(apperr.Timeout, "slow"), queue.OutcomeRetried},
		"unclassified is retried": {errors.New("who knows"), queue.OutcomeRetried},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pool := newPostgres(t)
			enqueue(t, pool, kind, 1)

			_, events := runPool(t, pool, testConfig(), map[string]queue.HandlerFunc{
				kind: func(context.Context, *queue.Job) error { return tc.err },
			})

			e := awaitOutcome(t, events, tc.want, 15*time.Second)
			if e.Outcome != tc.want {
				t.Errorf("outcome = %s, want %s", e.Outcome, tc.want)
			}
		})
	}
}

// Backpressure is not failure. A dependency being down must not spend the
// job's retry budget and dead-letter a job that was never wrong.
func TestSnoozeDoesNotConsumeAnAttempt(t *testing.T) {
	t.Parallel()
	pool := newPostgres(t)
	enqueue(t, pool, kind, 1)

	var calls atomic.Int64
	_, events := runPool(t, pool, testConfig(), map[string]queue.HandlerFunc{
		kind: func(context.Context, *queue.Job) error {
			if calls.Add(1) == 1 {
				return queue.Snooze(50*time.Millisecond, "circuit open")
			}
			return nil
		},
	})

	snoozed := awaitOutcome(t, events, queue.OutcomeSnoozed, 15*time.Second)
	row := readJob(t, pool, snoozed.JobID)
	if row.Attempt != 0 {
		t.Errorf("attempt = %d after a snooze, want 0: the claim's increment must be given back", row.Attempt)
	}
	if row.State != queue.StateScheduled {
		t.Errorf("state = %s, want scheduled", row.State)
	}

	// It comes back and succeeds on what is still attempt 1.
	done := awaitOutcome(t, events, queue.OutcomeSucceeded, 15*time.Second)
	if done.Attempt != 1 {
		t.Errorf("succeeded on attempt %d, want 1", done.Attempt)
	}
}

// A panic in one handler must not take the pool down, and the job that
// panicked deserves the same treatment as one that returned an error.
func TestPanicIsContainedAndRetried(t *testing.T) {
	t.Parallel()
	pool := newPostgres(t)
	enqueue(t, pool, kind, 1)

	var calls atomic.Int64
	_, events := runPool(t, pool, testConfig(), map[string]queue.HandlerFunc{
		kind: func(context.Context, *queue.Job) error {
			if calls.Add(1) == 1 {
				panic("boom")
			}
			return nil
		},
	})

	retried := awaitOutcome(t, events, queue.OutcomeRetried, 15*time.Second)
	if retried.Err == nil {
		t.Error("the panic was not recorded as the job's error")
	}

	// The pool survived and ran it again.
	done := awaitOutcome(t, events, queue.OutcomeSucceeded, 15*time.Second)
	if done.JobID != retried.JobID {
		t.Errorf("a different job succeeded: %d then %d", retried.JobID, done.JobID)
	}
}

// A kind nobody handles will not gain a handler by being retried. It is a
// deploy problem, and it should stop consuming attempts until it is fixed.
func TestUnknownKindIsTerminal(t *testing.T) {
	t.Parallel()
	pool := newPostgres(t)
	enqueue(t, pool, "nobody.handles.this", 1)

	_, events := runPool(t, pool, testConfig(), map[string]queue.HandlerFunc{
		kind: func(context.Context, *queue.Job) error { return nil },
	})

	e := awaitOutcome(t, events, queue.OutcomeCancelled, 15*time.Second)
	if e.Attempt != 1 {
		t.Errorf("cancelled on attempt %d, want 1", e.Attempt)
	}
}

// A handler with no deadline is precisely how a job gets stuck forever, and
// the reason the reaper has to exist.
func TestJobTimeoutCancelsTheHandler(t *testing.T) {
	t.Parallel()
	pool := newPostgres(t)
	enqueue(t, pool, kind, 1)

	cfg := testConfig()
	cfg.JobTimeout = 200_000_000 // 200ms

	_, events := runPool(t, pool, cfg, map[string]queue.HandlerFunc{
		kind: func(ctx context.Context, _ *queue.Job) error {
			<-ctx.Done()
			return ctx.Err()
		},
	})

	e := awaitOutcome(t, events, queue.OutcomeRetried, 15*time.Second)
	if e.Err == nil {
		t.Error("the timeout was not recorded as the job's error")
	}
}

// The pool alone cannot retry. A failed job goes to retryable and stays there
// until the scheduler moves it back to available — which is what stops a
// hot-failing job being instantly re-claimable and starving fresh work.
//
// The cost of that separation is a real operational dependency: a fleet where
// nothing runs maintenance stops retrying, silently. This is that dependency,
// written down.
func TestRetriesNeedTheScheduler(t *testing.T) {
	t.Parallel()
	pool := newPostgres(t)
	enqueue(t, pool, kind, 1)

	var attempts atomic.Int64
	_, events := runPoolOnly(t, pool, testConfig(), map[string]queue.HandlerFunc{
		kind: func(context.Context, *queue.Job) error {
			attempts.Add(1)
			return errors.New("fails once")
		},
	})

	retried := awaitOutcome(t, events, queue.OutcomeRetried, 15*time.Second)

	// Long enough for several poll intervals to pass without a scheduler.
	time.Sleep(500 * time.Millisecond) //nolint:forbidigo // proving absence needs a window

	if got := attempts.Load(); got != 1 {
		t.Errorf("handler ran %d times with no scheduler, want once", got)
	}
	if got := readJob(t, pool, retried.JobID).State; got != queue.StateRetryable {
		t.Errorf("state = %s, want retryable: nothing should have made it available", got)
	}
}
