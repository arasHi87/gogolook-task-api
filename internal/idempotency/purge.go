package idempotency

import (
	"context"
	"log/slog"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/logging"
)

// Purger deletes expired keys on a timer.
//
// Without it the table only grows: every write that carries a key adds a row
// that is never read again once its TTL has passed, and the primary key index
// grows with it. The TTL is the promise; this is what makes the promise
// affordable.
type Purger struct {
	store Store
	cfg   config.Idempotency
	log   *slog.Logger
}

// NewPurger wires the purge loop.
func NewPurger(store Store, cfg config.Idempotency, log *slog.Logger) *Purger {
	if log == nil {
		log = slog.Default()
	}
	return &Purger{
		store: store,
		cfg:   cfg,
		log:   log.With(slog.String("component", "idempotency-purge")),
	}
}

// Run purges until ctx is cancelled.
func (p *Purger) Run(ctx context.Context) error {
	ctx = logging.Into(ctx, p.log)

	tick := time.NewTicker(p.cfg.PurgeInterval.D())
	defer tick.Stop()

	p.log.Info("idempotency purge started",
		slog.Duration("interval", p.cfg.PurgeInterval.D()),
		slog.Duration("ttl", p.cfg.TTL.D()))

	for {
		select {
		case <-ctx.Done():
			p.log.Info("idempotency purge stopped")
			return nil
		case <-tick.C:
			p.purge(ctx)
		}
	}
}

func (p *Purger) purge(ctx context.Context) {
	n, err := p.store.Purge(ctx)
	if err != nil {
		// A failed purge costs disk, not correctness, so it is worth a line
		// but never worth failing the process over.
		if ctx.Err() == nil {
			p.log.Error("idempotency purge failed", slog.Any("err", err))
		}
		return
	}
	if n > 0 {
		p.log.Info("expired idempotency keys purged", slog.Int("count", n))
	}
}
