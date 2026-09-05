// Package queue is the Postgres-backed job queue.
//
// It is built rather than imported because the mechanism is what this exercise
// is testing: FOR UPDATE SKIP LOCKED, a lease with heartbeats, a stale-worker
// guard, and a transactional outbox. internal/queue/README.md records which of
// riverqueue/river's designs were adopted, which were declined, and the one
// place this goes further than it does.
//
// The delivery guarantee is at-least-once. Exactly-once *effects* are
// achievable and the handlers are written for it; exactly-once *delivery* is
// not, and claiming otherwise would be a lie.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// State is a job's position in its lifecycle. The values match the job_state
// enum in the schema.
type State string

const (
	// StateAvailable is claimable now.
	StateAvailable State = "available"
	// StateScheduled is waiting for its scheduled_at to arrive.
	StateScheduled State = "scheduled"
	// StateRunning is claimed by a worker holding a lease.
	StateRunning State = "running"
	// StateRetryable failed and is waiting for its backoff. It is deliberately
	// distinct from available: a failed job that goes straight back to
	// available is instantly re-claimable, hot-loops the pool and starves
	// fresh work.
	StateRetryable State = "retryable"
	// StateSucceeded is done.
	StateSucceeded State = "succeeded"
	// StateCancelled failed in a way that retrying cannot fix.
	StateCancelled State = "cancelled"
	// StateDiscarded exhausted its attempts. This is the dead-letter state.
	StateDiscarded State = "discarded"
)

// Job is a claimed unit of work.
type Job struct {
	ID          int64
	Kind        string
	Payload     json.RawMessage
	Attempt     int
	MaxAttempts int
	// Waited is how long this job sat between becoming due and being claimed,
	// measured by the database. It is the latency a producer experiences, and
	// the one thing queue depth cannot tell you: ten thousand jobs that drain
	// in twenty seconds and five that have been stuck for an hour look the
	// same by depth and nothing alike by this.
	Waited time.Duration
}

// HandlerFunc runs one job.
//
// What it returns decides what happens next, and the vocabulary is small on
// purpose:
//
//	nil                    succeeded
//	ErrTerminal (wrapped)  cancelled, no retry
//	SnoozeError            rescheduled, no attempt consumed
//	anything else          retried with backoff, or discarded at max attempts
//
// A handler must be idempotent. At-least-once delivery means it will
// eventually run twice for the same job, and a handler that cannot tolerate
// that is a handler that will eventually be wrong.
type HandlerFunc func(ctx context.Context, j *Job) error

// ErrTerminal marks a failure that retrying cannot fix.
//
// Wrapping with it is the single most valuable thing a handler can do.
// Retrying a poison job — a malformed payload, a webhook that returns 400 —
// five times is pure waste, and it hides the real bug behind a queue that
// looks merely slow.
var ErrTerminal = errors.New("terminal")

// IsTerminal reports whether err says the job must not be retried.
func IsTerminal(err error) bool { return errors.Is(err, ErrTerminal) }

// Terminal wraps err so the job is cancelled rather than retried.
func Terminal(err error) error {
	if err == nil {
		return ErrTerminal
	}
	return fmt.Errorf("%w: %w", ErrTerminal, err)
}

// SnoozeError reschedules a job without consuming an attempt.
//
// Backpressure is not failure. When a circuit breaker is open the dependency
// is down, not the job, and burning the job's attempt budget on someone else's
// outage is how a valid job ends up in the dead-letter state.
type SnoozeError struct {
	// For is how long to wait before the job becomes claimable again.
	For time.Duration
	// Reason is recorded in the job's error history.
	Reason string
}

func (e *SnoozeError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("snoozed for %s", e.For)
	}
	return fmt.Sprintf("snoozed for %s: %s", e.For, e.Reason)
}

// Snooze returns an error that reschedules the job without consuming an
// attempt.
func Snooze(d time.Duration, reason string) error {
	return &SnoozeError{For: d, Reason: reason}
}

// Outcome is what the pool decided to do with a finished job.
type Outcome string

const (
	// OutcomeSucceeded means the handler returned nil.
	OutcomeSucceeded Outcome = "succeeded"
	// OutcomeRetried means the job will be tried again after a backoff.
	OutcomeRetried Outcome = "retried"
	// OutcomeCancelled means the failure was terminal.
	OutcomeCancelled Outcome = "cancelled"
	// OutcomeDiscarded means the attempts ran out.
	OutcomeDiscarded Outcome = "discarded"
	// OutcomeSnoozed means the job was rescheduled without consuming an
	// attempt.
	OutcomeSnoozed Outcome = "snoozed"
	// OutcomeLost means the completion write matched no row: the lease had
	// already been reclaimed and someone else owns the job now.
	OutcomeLost Outcome = "lost"
)

// Event reports a finished job.
//
// Subscribing to these is what lets an integration test insert a job and wait
// for it, instead of sleeping for "long enough". A queue test suite full of
// time.Sleep is the one everybody ends up skipping.
type Event struct {
	JobID   int64
	Kind    string
	Attempt int
	Outcome Outcome
	Err     error
	// Waited is enqueue-to-claim; Took is claim-to-finalize. Both are on the
	// event rather than measured by a subscriber, because only the pool knows
	// where either interval starts.
	Waited time.Duration
	Took   time.Duration
}

// Observer receives the two things only the queue can see.
//
// An interface declared here and satisfied by the metrics registry, so the
// queue never imports it: instrumentation must not be able to make the thing
// it instruments depend on it.
type Observer interface {
	// Claimed is how many jobs one claim returned. Consistently short of the
	// configured batch is workers contending on SKIP LOCKED, which looks like
	// nothing else in the metrics.
	Claimed(n int)
	// LeasesExpired is what the reaper reclaimed from workers that died or
	// wedged holding work.
	LeasesExpired(retryable, discarded int)
}
