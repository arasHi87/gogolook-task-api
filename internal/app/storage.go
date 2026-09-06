package app

import (
	"context"
	"fmt"

	"github.com/arasHi87/gogolook-task-api/internal/admin"
	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/postgres"
	"github.com/arasHi87/gogolook-task-api/internal/task"
	"github.com/arasHi87/gogolook-task-api/internal/task/memrepo"
	"github.com/arasHi87/gogolook-task-api/internal/task/pgrepo"
)

// buildStorage opens the repository, and the pool behind it when there is one.
func (a *App) buildStorage(cfg *config.Config) error {
	repo, err := a.newRepository(cfg)
	if err != nil {
		return err
	}
	a.tasks = task.NewService(repo)
	return nil
}

// newRepository selects the storage backend.
//
// memory is the default and needs nothing: it is what makes `go run` work with
// no Postgres, no Docker and no configuration, which is the literal
// requirement the exercise states. postgres adds durability, the transactional
// outbox and the queue.
func (a *App) newRepository(cfg *config.Config) (task.Repository, error) {
	switch cfg.Storage.Backend {
	case config.BackendMemory:
		return memrepo.New(), nil

	case config.BackendPostgres:
		// The pool is opened here rather than lazily, so an unreachable
		// database is a startup failure with a clear message instead of a
		// confusing error on the first request.
		role := postgres.RoleAPI
		if a.mode == ModeWorker {
			role = postgres.RoleWorker
		}
		pool, err := postgres.OpenTraced(context.Background(), cfg.Storage.Postgres, role,
			cfg.Service.Instance, cfg.Observability.Tracing.Enabled)
		if err != nil {
			return nil, err
		}
		a.pool = pool
		a.closers = append(a.closers, func() error { pool.Close(); return nil })

		return pgrepo.New(pool, a.metrics.Queue), nil

	default:
		return nil, fmt.Errorf("unknown storage backend %q", cfg.Storage.Backend)
	}
}

// readinessChecks are the dependencies this process needs to serve.
//
// There are none on the memory backend, which is correct rather than lazy: it
// has no dependency that can be down.
func (a *App) readinessChecks() []admin.Check {
	if a.pool == nil {
		return nil
	}
	return []admin.Check{{
		Name: "postgres",
		// Ping, not a query: readiness asks whether a connection can be had,
		// and a SELECT would also be reporting on the query planner.
		Probe: a.pool.Ping,
	}}
}
