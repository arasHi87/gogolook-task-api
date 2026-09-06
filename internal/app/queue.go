package app

import (
	"fmt"
	"os"

	"github.com/google/uuid"

	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/queue"
	"github.com/arasHi87/gogolook-task-api/internal/queue/handler"
	"github.com/arasHi87/gogolook-task-api/internal/resilience"
	"github.com/arasHi87/gogolook-task-api/internal/task/pgrepo"
)

// buildQueue wires the consumer side, in the modes that consume.
//
// It needs a pool, so it is a no-op on the in-memory backend: the queue is the
// outbox, and an in-memory store has no transaction to write one in.
func (a *App) buildQueue(cfg *config.Config) error {
	if !a.mode.Consumes() || a.pool == nil {
		return nil
	}
	return a.newQueue(cfg)
}

// newQueue wires the consumer side: the listener that wakes it, the pool that
// claims and runs, and the maintenance loops one replica runs for everyone.
func (a *App) newQueue(cfg *config.Config) error {
	store := queue.NewStore(a.pool)

	a.listener = queue.NewListener(cfg.Storage.Postgres.DSN, a.log.Logger)
	a.maintenance = queue.NewMaintenance(a.pool, store, cfg.Queue, a.log.Logger, a.metrics.Queue)

	// The webhook is the only genuinely remote thing this service talks to,
	// which makes it the only honest place for a circuit breaker.
	webhook := handler.NewWebhook(handler.Options{
		URL:      cfg.Webhook.URL,
		Timeout:  cfg.Webhook.Timeout.D(),
		Observer: a.metrics.Deps,
		Trace:    cfg.Observability.Tracing.Enabled,
		Guard: resilience.New(resilience.Options{
			Name:      "webhook",
			Breaker:   cfg.Breaker.Webhook,
			Timeout:   cfg.Webhook.Timeout.D(),
			Retry:     cfg.Webhook.Retry,
			Logger:    a.log.Logger,
			IsFailure: handler.IsDependencyFailure,
			Observer:  a.metrics.Guards,
		}),
	})

	jobs, err := queue.NewPool(queue.PoolOptions{
		Store: store,
		Handlers: map[string]queue.HandlerFunc{
			pgrepo.JobKind: webhook.Handle,
		},
		// locked_by has to be unique per process, or the stale-worker guard
		// cannot tell two replicas apart and a zombie can overwrite a live
		// worker's result.
		WorkerID: workerID(cfg.Service.Instance),
		Config:   cfg.Queue,
		Logger:   a.log.Logger,
		Notify:   a.listener.Notifications(),
		Observer: a.metrics.Queue,
		Tracer:   a.tracing.Tracer(),
	})
	if err != nil {
		return err
	}
	a.jobs = jobs
	a.metrics.Queue.Configured(cfg.Queue.Workers)
	return nil
}

// workerID identifies this process in the queue.
//
// The instance alone is not enough: two processes on one host, or a container
// restarted under the same name, would share an id and each could then finalize
// the other's jobs.
func workerID(instance string) string {
	return fmt.Sprintf("%s/%d/%s", instance, os.Getpid(), uuid.NewString()[:8])
}
