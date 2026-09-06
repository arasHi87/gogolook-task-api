package app

import (
	"context"
	"fmt"
	"io"

	"github.com/arasHi87/gogolook-task-api/internal/admin"
	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/metrics"
	"github.com/arasHi87/gogolook-task-api/internal/queue"
	"github.com/arasHi87/gogolook-task-api/internal/tracing"
)

// buildObservability constructs the metric registry and the tracer.
//
// It is the first build step, because everything constructed after it reports
// into one or the other, and a collector handed to a component after that
// component was built is a collector nobody writes to.
func (a *App) buildObservability(cfg *config.Config) error {
	a.metrics = metrics.New(cfg.Observability.Metrics)

	traces, err := tracing.New(
		cfg.Observability.Tracing,
		cfg.Service.Name, cfg.Service.Instance, a.log.Logger,
	)
	if err != nil {
		return err
	}
	a.tracing = traces
	return nil
}

// debug assembles the operator surface for the admin listener.
//
// Everything here is on the private port and nowhere else. A heap profile is a
// memory dump and the config dump names every dependency, so which port they
// are on is the security boundary rather than a convention.
func (a *App) debug() admin.Debug {
	d := admin.Debug{
		Level: a.log,
		Pprof: a.Config().Admin.Pprof,
		// The same renderer --print-config uses, so the two cannot disagree
		// about what is masked.
		Config: func(w io.Writer) error { return a.Config().WriteYAML(w) },
	}
	if a.metrics.Enabled() {
		d.Metrics = a.metrics.Handler()
	}
	return d
}

// registerCollectors adds the metrics that have to query something on scrape.
//
// They are registered here rather than inside internal/metrics because they
// need a pool and a store, and a metrics package that reached for a database
// would be instrumenting by owning.
func (a *App) registerCollectors(*config.Config) error {
	if a.pool == nil {
		return nil
	}

	role := "api"
	if a.mode == ModeWorker {
		role = "worker"
	}
	if err := a.metrics.Register(metrics.NewPoolCollector(a.pool, role)); err != nil {
		return fmt.Errorf("register pool collector: %w", err)
	}

	// Only where the queue runs. A process that does not consume would report
	// a backlog it has nothing to do with, and two replicas reporting the same
	// global gauge is a sum that double counts.
	if a.mode.Consumes() {
		if err := a.metrics.Register(metrics.NewQueueCollector(queue.NewStore(a.pool), a.log.Logger)); err != nil {
			return fmt.Errorf("register queue collector: %w", err)
		}
	}
	return nil
}

// recordJobOutcomes turns the pool's completion events into metrics.
//
// A subscriber rather than instrumentation inside the pool, because the event
// already exists and already carries everything needed: the outcome, the wait
// from enqueue to claim, and the time in the handler. Adding a second
// reporting path inside the pool would be two places to keep in step.
//
// Delivery is non-blocking on the publisher's side, so a slow drain here drops
// events rather than stalling a worker. That is the right trade for metrics
// and the wrong one for the queue, which is why it is the queue's choice.
func (a *App) recordJobOutcomes(ctx context.Context) error {
	events, unsubscribe := a.jobs.Subscribe(256)
	defer unsubscribe()

	for {
		select {
		case <-ctx.Done():
			return nil
		case e, ok := <-events:
			if !ok {
				return nil
			}
			a.metrics.Queue.Finished(e)
		}
	}
}
