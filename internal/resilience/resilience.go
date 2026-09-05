// Package resilience builds the policy chain around an outbound call.
//
// A circuit breaker points the opposite way from a rate limiter: this guards
// against a dependency, the limiter guards against callers. They are not
// substitutes, and putting either one on the other's side is a common mistake
// that produces something that looks protected and is not.
//
// The chain is ordered, and the order changes the semantics completely — which
// is the reason it is composed by a library rather than by hand:
//
//	retry( breaker( timeout( call ) ) )
//
// Timeout innermost, because a breaker with no per-attempt timeout never trips:
// the calls do not fail, they hang, and a hang is not a failure the breaker can
// count.
//
// Breaker inside the retry, so every attempt is accounted. The other way round
// — one breaker execution per logical call, whatever it took — hides three
// failures behind one data point, and a dependency that is completely down
// takes three times as long to trip the circuit as it should. It also means
// the retries keep hammering a dependency the breaker has already given up on.
// This is the order failsafe-go's own examples use and the one Resilience4j
// documents; it is easy to get backwards, and backwards still compiles.
//
// An open circuit aborts the retry loop rather than being retried. Retrying a
// refusal that is designed to be instant is a busy loop with extra steps.
package resilience

import (
	"context"
	"log/slog"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/budget"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
	"github.com/failsafe-go/failsafe-go/timeout"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

// Options describe one protected dependency.
type Options struct {
	// Name appears in the log lines and, later, in the metric labels. One
	// breaker per target, never a global one: a slow analytics endpoint must
	// not be able to trip the circuit protecting the payment one.
	Name    string
	Breaker config.BreakerSettings
	// Timeout bounds one attempt.
	Timeout time.Duration
	// Retry is the per-call retry policy, applied inside the breaker.
	Retry  config.WebhookRetry
	Logger *slog.Logger

	// IsFailure decides what the breaker counts. It is a parameter rather than
	// a rule here because only the caller knows which of its errors are the
	// dependency's fault.
	//
	// Getting this wrong is the other half of a misconfigured breaker, and it
	// is usually left wrong: counting a 4xx means one validation bug takes
	// down a healthy dependency, because our malformed payload looks exactly
	// like their outage.
	IsFailure func(error) bool
}

// Executor runs a call under the policy chain.
type Executor struct {
	name     string
	breaker  circuitbreaker.CircuitBreaker[any]
	executor failsafe.Executor[any]
}

// New builds the chain.
func New(o Options) *Executor {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With(slog.String("component", "breaker"), slog.String("target", o.Name))

	isFailure := o.IsFailure
	if isFailure == nil {
		isFailure = func(err error) bool { return err != nil }
	}

	breaker := newBreaker(o, log, isFailure)
	retry := newRetry(o, isFailure)
	attempt := timeout.New[any](o.Timeout)

	// Outermost first. failsafe applies the policies left to right on the way
	// in, so this reads in the same order as the comment on the package.
	return &Executor{
		name:     o.Name,
		breaker:  breaker,
		executor: failsafe.With[any](retry, breaker, attempt),
	}
}

func newBreaker(o Options, log *slog.Logger, isFailure func(error) bool) circuitbreaker.CircuitBreaker[any] {
	b := o.Breaker

	return circuitbreaker.NewBuilder[any]().
		// Rate-based rather than count-based: a threshold of "five failures"
		// means something different at ten requests per second than at a
		// thousand, and the floor below it is what stops one failure out of
		// one request opening the circuit.
		WithFailureRateThreshold(b.FailureRateThreshold, b.MinThroughput, b.Window.D()).
		WithDelay(b.OpenDuration.D()).
		WithSuccessThresholdRatio(b.HalfOpenSuccessThreshold, b.HalfOpenMaxCalls).
		HandleIf(func(_ any, err error) bool { return isFailure(err) }).
		OnStateChanged(func(e circuitbreaker.StateChangedEvent) {
			// A breaker that opens silently is a breaker nobody knows about.
			// This is the line that turns "the webhook stopped being called"
			// from a mystery into a fact with a timestamp.
			//
			// One message per destination state, not one shared message with a
			// "to" attribute. The log throttler collapses repeated WARN and
			// ERROR records by (level, message) — which is right for a breaker
			// that flaps, and silently ate the recovery when every transition
			// said the same thing. Distinct messages mean flapping is still
			// collapsed per state while open and closed are never confused for
			// each other.
			level, msg := transitionLine(e.NewState)
			log.LogAttrs(context.Background(), level, msg,
				slog.String("from", e.OldState.String()),
				slog.String("to", e.NewState.String()))
		}).
		Build()
}

// transitionLine names a state change.
//
// Opening is a warning: a dependency is failing and calls are being refused.
// The other two are not — half-open is the mechanism working, and closed is
// good news. An operator who greps WARN for problems should not find a
// recovery there.
func transitionLine(to circuitbreaker.State) (slog.Level, string) {
	switch to {
	case circuitbreaker.OpenState:
		return slog.LevelWarn, "circuit breaker opened"
	case circuitbreaker.HalfOpenState:
		return slog.LevelInfo, "circuit breaker probing"
	default:
		return slog.LevelInfo, "circuit breaker closed"
	}
}

func newRetry(o Options, isFailure func(error) bool) retrypolicy.RetryPolicy[any] {
	r := o.Retry

	return retrypolicy.NewBuilder[any]().
		WithMaxAttempts(r.MaxAttempts).
		WithBackoff(r.Base.D(), r.Max.D()).
		// Plus or minus half the delay. Not the full jitter the job-level
		// backoff uses — failsafe's factor varies the delay symmetrically
		// rather than sampling from zero, so a factor of 1 would allow a
		// near-immediate retry against a dependency that has just failed,
		// which is the worst available choice. Half is enough decorrelation to
		// stop a fleet reconverging on one instant.
		WithJitterFactor(0.5).
		// An open circuit is not a failure worth retrying: the refusal is
		// immediate by design, so retrying it three times is a busy loop that
		// arrives at the same answer. Abort and let the caller decide — which
		// here means snoozing the job rather than burning its attempt budget
		// on someone else's outage.
		AbortOnErrors(circuitbreaker.ErrOpen).
		// The retry budget, and the reason it is here rather than left to
		// max_attempts: without one, a total outage costs the dependency
		// max_attempts times its normal traffic exactly when it is least able
		// to take it. With one, retries are capped as a fraction of what is
		// already in flight, so an outage costs a little more traffic instead
		// of several times as much.
		//
		// It bounds concurrent retries rather than a rate over time, which is
		// a slightly different quantity from the Google SRE formulation and
		// the same idea. min_concurrency is the library's 3: a floor, so a
		// service with almost no traffic can still retry at all.
		WithBudget(budget.NewBuilder().
			WithMaxRate(r.BudgetMaxRate).
			WithMinConcurrency(3).
			Build()).
		HandleIf(func(_ any, err error) bool { return isFailure(err) }).
		Build()
}

// Run executes fn under the chain.
//
// The error it returns is either fn's own or circuitbreaker.ErrOpen, and the
// caller is expected to tell them apart: they mean different things and want
// different handling. A failed call is the job's problem; an open circuit is
// the fleet's, and burning a job's retry budget on it would discard valid work
// for an outage that has nothing to do with it.
func (e *Executor) Run(fn func() error) error {
	return e.executor.Run(fn)
}

// Open reports whether the circuit is currently refusing calls.
func (e *Executor) Open() bool { return e.breaker.IsOpen() }

// State is the breaker's state, for logs and health output.
func (e *Executor) State() string { return e.breaker.State().String() }

// RemainingDelay is how long until the circuit will probe again. It is what a
// snooze should wait for: rescheduling a job to run before the circuit can
// even try is a guaranteed second failure.
func (e *Executor) RemainingDelay() time.Duration { return e.breaker.RemainingDelay() }

// Name identifies the protected target.
func (e *Executor) Name() string { return e.name }
