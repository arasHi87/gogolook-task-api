package resilience_test

import (
	"errors"
	"testing"
	"time"

	"github.com/failsafe-go/failsafe-go/circuitbreaker"

	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/resilience"
)

var (
	errDependency = errors.New("the dependency is down")
	errOurs       = errors.New("our payload is wrong")
)

// isDependencyFailure is the rule under test in half of these: our own bugs
// must not open a circuit protecting a healthy dependency.
func isDependencyFailure(err error) bool {
	return err != nil && !errors.Is(err, errOurs)
}

func testOptions() resilience.Options {
	cfg := config.Defaults()

	b := cfg.Breaker.Webhook
	b.MinThroughput = 4
	b.Window = config.Duration(time.Minute)
	b.OpenDuration = config.Duration(50 * time.Millisecond)
	b.HalfOpenMaxCalls = 2
	b.HalfOpenSuccessThreshold = 2

	r := cfg.Webhook.Retry
	r.MaxAttempts = 1 // retries are a separate test; most cases want one call
	r.Base = config.Duration(time.Millisecond)
	r.Max = config.Duration(2 * time.Millisecond)

	return resilience.Options{
		Name:      "test",
		Breaker:   b,
		Timeout:   time.Second,
		Retry:     r,
		IsFailure: isDependencyFailure,
	}
}

// The floor is the single most common misconfiguration of a breaker: without
// it, one failure out of one request opens the circuit.
func TestTheCircuitOpensOnlyAboveTheThroughputFloor(t *testing.T) {
	t.Parallel()

	e := resilience.New(testOptions())

	// Three failures, one below the floor of four.
	for range 3 {
		_ = e.Run(func() error { return errDependency })
	}
	if e.Open() {
		t.Fatalf("the circuit opened after 3 failures, below the throughput floor of 4")
	}

	_ = e.Run(func() error { return errDependency })
	if !e.Open() {
		t.Errorf("the circuit is %s after 4 failures at a 50%% threshold, want open", e.State())
	}
}

// The other half of a breaker's configuration, and the half usually left
// wrong. Counting a rejected payload means one validation bug takes down a
// healthy dependency, because our malformed request looks exactly like their
// outage.
func TestOurOwnErrorsDoNotOpenTheCircuit(t *testing.T) {
	t.Parallel()

	e := resilience.New(testOptions())

	for range 20 {
		if err := e.Run(func() error { return errOurs }); !errors.Is(err, errOurs) {
			t.Fatalf("Run = %v, want the handler's own error", err)
		}
	}
	if e.Open() {
		t.Error("twenty rejected payloads opened the circuit protecting a healthy dependency")
	}
}

// An open circuit refuses instantly, which is the point: the caller finds out
// in microseconds instead of waiting for a timeout it already knows will fire.
func TestAnOpenCircuitRefusesWithoutCalling(t *testing.T) {
	t.Parallel()

	e := resilience.New(testOptions())
	for range 4 {
		_ = e.Run(func() error { return errDependency })
	}

	called := false
	err := e.Run(func() error { called = true; return nil })

	if !errors.Is(err, circuitbreaker.ErrOpen) {
		t.Errorf("Run = %v, want ErrOpen", err)
	}
	if called {
		t.Error("the dependency was called through an open circuit")
	}
	if e.RemainingDelay() <= 0 {
		t.Error("RemainingDelay is zero; a snooze would reschedule before the circuit can probe")
	}
}

// One lucky success is not recovery, which is why the half-open threshold is
// more than one.
func TestRecoveryNeedsMoreThanOneSuccess(t *testing.T) {
	t.Parallel()

	e := resilience.New(testOptions())
	for range 4 {
		_ = e.Run(func() error { return errDependency })
	}

	// Wait out the open duration so the next call is a probe.
	deadline := time.Now().Add(5 * time.Second)
	for e.Open() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if err := e.Run(func() error { return nil }); err != nil {
		t.Fatalf("first probe = %v, want nil", err)
	}
	if e.State() == "closed" {
		t.Error("the circuit closed on one probe; the success threshold is two")
	}
	if err := e.Run(func() error { return nil }); err != nil {
		t.Fatalf("second probe = %v, want nil", err)
	}
	if e.State() != "closed" {
		t.Errorf("state = %s after two successful probes, want closed", e.State())
	}
}

// Retries run inside the breaker, so every attempt is accounted. The other
// order hides three failures behind one data point and a dependency that is
// completely down takes three times as long to trip the circuit.
func TestEveryRetryAttemptIsCounted(t *testing.T) {
	t.Parallel()

	o := testOptions()
	o.Retry.MaxAttempts = 4
	e := resilience.New(o)

	attempts := 0
	_ = e.Run(func() error { attempts++; return errDependency })

	if attempts < 4 {
		t.Errorf("%d attempts, want the configured 4", attempts)
	}
	// Four attempts is exactly the floor, so one logical call is enough to
	// trip the circuit — which is the property being asserted.
	if !e.Open() {
		t.Errorf("the circuit is %s after four failed attempts, want open", e.State())
	}
}

// Retrying a refusal designed to be instant is a busy loop that arrives at the
// same answer.
func TestRetriesStopAtAnOpenCircuit(t *testing.T) {
	t.Parallel()

	o := testOptions()
	o.Retry.MaxAttempts = 10
	o.Retry.Base = config.Duration(50 * time.Millisecond)
	e := resilience.New(o)

	for range 4 {
		_ = e.Run(func() error { return errDependency })
	}

	started := time.Now()
	err := e.Run(func() error { return nil })

	if !errors.Is(err, circuitbreaker.ErrOpen) {
		t.Errorf("Run = %v, want ErrOpen", err)
	}
	// Ten retries at 50ms would take half a second. Aborting takes none.
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Errorf("took %s; the open circuit was retried instead of aborting", elapsed)
	}
}

// A breaker with no per-attempt timeout never trips: the calls do not fail,
// they hang, and a hang is not a failure the breaker can count.
func TestASlowCallIsAFailure(t *testing.T) {
	t.Parallel()

	o := testOptions()
	o.Timeout = 20 * time.Millisecond
	e := resilience.New(o)

	err := e.Run(func() error {
		time.Sleep(200 * time.Millisecond)
		return nil
	})
	if err == nil {
		t.Fatal("Run = nil, want a timeout")
	}
}
