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

	var (
		wg     sync.WaitGroup
		errMu  sync.Mutex
		first  error
		failed = make(chan struct{})
		once   sync.Once
	)

	record := func(err error) {
		errMu.Lock()
		if first == nil {
			first = err
		}
		errMu.Unlock()
		once.Do(func() { close(failed) })
	}

	for _, w := range workers {
		wg.Add(1)
		go func(w Worker) {
			defer wg.Done()
			defer func() {
				// A panic in one worker must not take the process down without
				// a word about which one it was.
				if r := recover(); r != nil {
					log.Error("worker panicked", slog.String("worker", w.Name), slog.Any("panic", r))
					record(fmt.Errorf("%s: panic: %v", w.Name, r))
				}
			}()

			log.Debug("worker started", slog.String("worker", w.Name))
			err := w.Run(runCtx)
			switch {
			case err == nil, errors.Is(err, context.Canceled):
				log.Debug("worker stopped", slog.String("worker", w.Name))
			default:
				log.Error("worker failed", slog.String("worker", w.Name), slog.Any("err", err))
				record(fmt.Errorf("%s: %w", w.Name, err))
			}
		}(w)
	}

	select {
	case <-ctx.Done():
		log.Info("shutdown signalled, draining")
	case <-failed:
		log.Error("worker failure, draining")
	}

	if err := drain(ctx, log, workers, &wg, cancel, grace); err != nil {
		record(err)
	}

	errMu.Lock()
	defer errMu.Unlock()
	return first
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
