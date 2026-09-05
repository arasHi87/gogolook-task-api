package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// Graceful shutdown is provable, not hopeful: if any worker goroutine
	// outlives its Run, this fails the package.
	goleak.VerifyTestMain(m)
}

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// order records the sequence of lifecycle events across goroutines.
type order struct {
	mu     sync.Mutex
	events []string
}

func (o *order) add(s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, s)
}

func (o *order) list() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.events...)
}

// blocker is a worker that runs until its context is cancelled.
func blocker(name string, o *order) Worker {
	return Worker{
		Name: name,
		Run: func(ctx context.Context) error {
			<-ctx.Done()
			o.add("run-done:" + name)
			return ctx.Err()
		},
	}
}

// The table is the shutdown sequence. Stop is called top to bottom, and every
// Stop completes before any Run context is cancelled — that is what lets the
// HTTP listener stop accepting before the queue stops consuming.
func TestDrainFollowsTableOrder(t *testing.T) {
	o := &order{}

	stopper := func(name string) Worker {
		w := blocker(name, o)
		w.Stop = func(context.Context) error {
			o.add("stop:" + name)
			return nil
		}
		return w
	}

	workers := []Worker{stopper("api"), stopper("queue"), blocker("reaper", o)}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()

	if err := runWorkers(ctx, discard(), workers, time.Second); err != nil {
		t.Fatalf("runWorkers: %v", err)
	}
	defer cancel()

	got := o.list()
	if len(got) != 5 {
		t.Fatalf("events = %v, want 5", got)
	}
	if got[0] != "stop:api" || got[1] != "stop:queue" {
		t.Errorf("events = %v, want the Stop calls first and in table order", got)
	}
	for _, e := range got[2:] {
		if !strings.HasPrefix(e, "run-done:") {
			t.Errorf("events = %v, want every Run to finish after every Stop", got)
		}
	}
}

// A worker that fails takes the process down rather than leaving it half
// running: a queue consumer that died silently is worse than a crash loop.
func TestWorkerFailureTriggersShutdown(t *testing.T) {
	o := &order{}
	boom := errors.New("listener failed to bind")

	workers := []Worker{
		blocker("healthy", o),
		{Name: "broken", Run: func(context.Context) error { return boom }},
	}

	err := runWorkers(context.Background(), discard(), workers, time.Second)
	if !errors.Is(err, boom) {
		t.Fatalf("runWorkers = %v, want the worker's error", err)
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Errorf("error = %v, want it to name the failing worker", err)
	}
	if got := o.list(); len(got) != 1 || got[0] != "run-done:healthy" {
		t.Errorf("events = %v, want the healthy worker to have been drained too", got)
	}
}

// Context cancellation is the normal path and is not an error.
func TestCancellationIsNotAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	o := &order{}
	if err := runWorkers(ctx, discard(), []Worker{blocker("a", o)}, time.Second); err != nil {
		t.Errorf("runWorkers = %v, want nil for a clean shutdown", err)
	}
}

// A worker that ignores its context must not hang the process forever. The
// grace deadline bounds shutdown, and the failure is reported rather than
// swallowed.
func TestShutdownIsBoundedByGrace(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	stuck := Worker{
		Name: "stuck",
		Run: func(context.Context) error {
			<-release // deliberately ignores ctx
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	started := time.Now()
	err := runWorkers(ctx, discard(), []Worker{stuck}, 50*time.Millisecond)
	elapsed := time.Since(started)

	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("runWorkers = %v, want a timeout error", err)
	}
	if elapsed > time.Second {
		t.Errorf("shutdown took %v, want it bounded by the grace period", elapsed)
	}
}

// A panic in one worker must be attributed and must not take the others with
// it silently.
func TestPanicIsContainedAndReported(t *testing.T) {
	o := &order{}
	workers := []Worker{
		blocker("healthy", o),
		{Name: "panicky", Run: func(context.Context) error { panic("boom") }},
	}

	err := runWorkers(context.Background(), discard(), workers, time.Second)
	if err == nil || !strings.Contains(err.Error(), "panicky") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("runWorkers = %v, want an error naming the panicking worker and its value", err)
	}
	if got := o.list(); len(got) != 1 {
		t.Errorf("events = %v, want the healthy worker drained cleanly", got)
	}
}

// A failing Stop is reported but must not prevent the rest of the drain.
func TestDrainContinuesPastAFailingStop(t *testing.T) {
	o := &order{}
	bad := blocker("bad", o)
	bad.Stop = func(context.Context) error { return errors.New("shutdown refused") }
	good := blocker("good", o)
	good.Stop = func(context.Context) error { o.add("stop:good"); return nil }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := runWorkers(ctx, discard(), []Worker{bad, good}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "shutdown refused") {
		t.Errorf("runWorkers = %v, want the Stop failure surfaced", err)
	}
	found := false
	for _, e := range o.list() {
		if e == "stop:good" {
			found = true
		}
	}
	if !found {
		t.Error("a failing Stop must not abort the rest of the drain")
	}
}
