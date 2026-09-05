package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Worker is one long-lived goroutine owned by the composition root.
type Worker struct {
	// Name appears in the startup and drain log lines.
	Name string
	// Run blocks until ctx is cancelled or the worker fails. Returning a
	// context cancellation error is not a failure.
	Run func(ctx context.Context) error
	// Stop, when set, is asked to drain gracefully before the run context is
	// cancelled. http.Server needs this: cancelling its context would drop
	// in-flight requests, while Shutdown finishes them.
	Stop func(ctx context.Context) error
}

// runWorkers starts every worker, blocks until ctx is cancelled or one of them
// fails, then drains in table order and returns the first error seen.
//
// It is a free function rather than a method so the lifecycle can be tested
// against synthetic workers: the table in App.workers is data, this is the
// machinery that runs it.
func runWorkers(ctx context.Context, log *slog.Logger, workers []Worker, grace time.Duration) error {
	// Detached from ctx on purpose. If the workers ran on a context derived
	// from the signal context, SIGTERM would cancel them the instant it
	// arrived — before any Stop was called — and the graceful drain below
	// would be decorative: every Stop would be draining a worker that had
	// already returned. Worker lifetime is controlled by the drain sequence,
	// which cancels this context once Stop has had its turn.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()

	failures := newFailureSet()
	wg := start(runCtx, log, workers, failures)

	select {
	case <-ctx.Done():
		log.Info("shutdown signalled, draining")
	case <-failures.fired:
		log.Error("worker failure, draining")
	}

	if err := drain(ctx, log, workers, wg, cancel, grace); err != nil {
		failures.record(err)
	}
	return failures.first()
}

// start launches every worker and reports what each one did when it returned.
func start(ctx context.Context, log *slog.Logger, workers []Worker, failures *failureSet) *sync.WaitGroup {
	var wg sync.WaitGroup

	for _, w := range workers {
		wg.Add(1)
		go func(w Worker) {
			defer wg.Done()
			defer func() {
				// A panic in one worker must not take the process down without
				// a word about which one it was.
				if r := recover(); r != nil {
					log.Error("worker panicked", slog.String("worker", w.Name), slog.Any("panic", r))
					failures.record(fmt.Errorf("%s: panic: %v", w.Name, r))
				}
			}()

			log.Debug("worker started", slog.String("worker", w.Name))
			err := w.Run(ctx)

			switch {
			case err == nil, errors.Is(err, context.Canceled):
				log.Debug("worker stopped", slog.String("worker", w.Name))
			default:
				log.Error("worker failed", slog.String("worker", w.Name), slog.Any("err", err))
				failures.record(fmt.Errorf("%s: %w", w.Name, err))
			}
		}(w)
	}
	return &wg
}

// failureSet keeps the first error and signals once that something went wrong.
//
// It exists so the shutdown trigger ("has anything failed?") and the exit code
// ("what failed first?") are one concept with one lock, instead of a mutex, a
// channel and a sync.Once threaded through the function that uses them.
type failureSet struct {
	mu    sync.Mutex
	err   error
	once  sync.Once
	fired chan struct{}
}

func newFailureSet() *failureSet {
	return &failureSet{fired: make(chan struct{})}
}

// record keeps err if it is the first, and wakes whoever is waiting.
func (f *failureSet) record(err error) {
	f.mu.Lock()
	if f.err == nil {
		f.err = err
	}
	f.mu.Unlock()
	f.once.Do(func() { close(f.fired) })
}

// first returns the earliest error recorded, or nil.
func (f *failureSet) first() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// drain walks the worker table in order, giving each a chance to finish what it
// already accepted before the run context is cancelled underneath it.
//
// Table order is drain order, and it is the whole point of the table: stop
// accepting new work, then finish what was accepted, then let the maintenance
// loops go. The deadline comes from http.shutdown_grace and is shared by the
// entire sequence, so one wedged worker cannot extend shutdown without bound.
func drain(
	parent context.Context,
	log *slog.Logger,
	workers []Worker,
	wg *sync.WaitGroup,
	cancel context.CancelFunc,
	grace time.Duration,
) error {
	// The parent context is usually already cancelled by the signal that got us
	// here, so drain runs on a detached one with its own deadline.
	drainCtx, done := context.WithTimeout(context.WithoutCancel(parent), grace)
	defer done()

	started := time.Now()
	var firstErr error
	for _, w := range workers {
		if w.Stop == nil {
			continue
		}
		log.Debug("draining", slog.String("worker", w.Name))
		if err := w.Stop(drainCtx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("drain %s: %w", w.Name, err)
			log.Error("drain failed", slog.String("worker", w.Name), slog.Any("err", err))
		}
	}

	// Everything that stops on context cancellation alone stops here.
	cancel()

	stopped := make(chan struct{})
	go func() { wg.Wait(); close(stopped) }()

	select {
	case <-stopped:
		log.Info("shutdown complete", slog.Duration("took", time.Since(started)))
	case <-drainCtx.Done():
		// Report it rather than hang: a worker that ignores its context is a
		// bug, and this log line is how it gets found.
		log.Error("shutdown timed out, exiting with work still in flight",
			slog.Duration("grace", grace))
		if firstErr == nil {
			firstErr = fmt.Errorf("shutdown timed out after %s", grace)
		}
	}
	return firstErr
}
