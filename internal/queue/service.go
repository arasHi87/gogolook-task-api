package queue

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/logging"
)

// Maintenance runs the fleet-wide loops: the scheduler, the reaper and the
// purger.
//
// Archetype: Service. It is separate from the pool because it is a different
// kind of work — one replica does it for everyone, on a timer, rather than
// every replica doing it for itself.
type Maintenance struct {
	pool    *pgxpool.Pool
	store   *Store
	cfg     config.Queue
	log     *slog.Logger
	backoff Backoff
}

// NewMaintenance wires the maintenance loops.
func NewMaintenance(pool *pgxpool.Pool, store *Store, cfg config.Queue, log *slog.Logger) *Maintenance {
	if log == nil {
		log = slog.Default()
	}
	return &Maintenance{
		pool:    pool,
		store:   store,
		cfg:     cfg,
		log:     log.With(slog.String("component", "queue-maintenance")),
		backoff: NewBackoff(cfg.Backoff),
	}
}

// Run ticks the maintenance loops until ctx is cancelled.
func (m *Maintenance) Run(ctx context.Context) error {
	ctx = logging.Into(ctx, m.log)

	// The scheduler runs on the same tick as the reaper: both are cheap index
	// scans, and one timer is easier to reason about than three.
	sweep := time.NewTicker(m.cfg.ReaperInterval.D())
	defer sweep.Stop()

	purge := time.NewTicker(m.cfg.Retention.PurgeInterval.D())
	defer purge.Stop()

	m.log.Info("maintenance started",
		slog.Duration("sweep", m.cfg.ReaperInterval.D()),
		slog.Duration("purge", m.cfg.Retention.PurgeInterval.D()))

	for {
		select {
		case <-ctx.Done():
			m.log.Info("maintenance stopped")
			return nil
		case <-sweep.C:
			m.Sweep(ctx)
		case <-purge.C:
			m.Purge(ctx)
		}
	}
}

// Sweep makes due jobs claimable and reclaims expired leases, on the leader.
func (m *Maintenance) Sweep(ctx context.Context) {
	m.asLeader(ctx, "queue-sweep", func(ctx context.Context) error {
		// Scheduling first: a job whose backoff has elapsed should be
		// claimable in the same tick that notices it, not the next one.
		if n, err := m.store.Schedule(ctx); err != nil {
			return err
		} else if n > 0 {
			m.log.Debug("jobs made available", slog.Int("count", n))
		}

		retryable, discarded, err := m.store.Reap(ctx, m.backoff, m.cfg.MaxAttempts)
		if err != nil {
			return err
		}
		if retryable+discarded > 0 {
			// A lease expiring means a worker died or wedged holding a job.
			// It is self-healing, which makes it a warning rather than an
			// error, but it is never routine.
			m.log.Warn("expired leases reclaimed",
				slog.Int("retryable", retryable), slog.Int("discarded", discarded))
		}
		return nil
	})
}

// Purge deletes finalized jobs past their retention, on the leader.
func (m *Maintenance) Purge(ctx context.Context) {
	m.asLeader(ctx, "queue-purge", func(ctx context.Context) error {
		n, err := m.store.Purge(ctx, m.cfg.Retention.Succeeded.D(), m.cfg.Retention.Discarded.D())
		if err != nil {
			return err
		}
		if n > 0 {
			m.log.Info("finalized jobs purged", slog.Int("count", n))
		}
		return nil
	})
}

// asLeader runs fn under an advisory lock, so exactly one replica does the
// work per tick.
func (m *Maintenance) asLeader(ctx context.Context, name string, fn func(context.Context) error) {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Error("maintenance could not begin", slog.String("sweep", name), slog.Any("err", err))
		}
		return
	}
	//nolint:errcheck // a rollback after commit is a no-op
	defer tx.Rollback(ctx)

	led, err := WithLeader(ctx, tx, name, fn)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Error("maintenance failed", slog.String("sweep", name), slog.Any("err", err))
		}
		return
	}
	if !led {
		// Another replica has it. Not an error, and not worth a line above
		// TRACE: it is the expected outcome on every replica but one.
		logging.Trace(ctx, "maintenance skipped, another replica is leader", slog.String("sweep", name))
		return
	}

	if err := tx.Commit(ctx); err != nil && ctx.Err() == nil {
		m.log.Error("maintenance could not commit", slog.String("sweep", name), slog.Any("err", err))
	}
}

var _ = errors.Is
var _ = pgx.ErrNoRows
