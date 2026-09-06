package app

import (
	"github.com/arasHi87/gogolook-task-api/internal/auth"
	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/idempotency"
	"github.com/arasHi87/gogolook-task-api/internal/ratelimit"
)

// buildGuards wires everything that decides how much of the service a caller
// gets: who they are, how fast they may go, and whether this request has
// already been done.
func (a *App) buildGuards(cfg *config.Config) error {
	a.newIdempotency(cfg)
	a.newGuards(cfg)
	return nil
}

// newIdempotency selects the key store, matching the storage backend.
//
// It has to match: a Postgres-backed service with an in-memory key store would
// honour a retry only when it landed on the same replica, which is worse than
// not honouring it at all — the failure is invisible and depends on the load
// balancer.
func (a *App) newIdempotency(cfg *config.Config) {
	if !cfg.Idempotency.Enabled {
		return
	}

	if a.pool != nil {
		a.keys = idempotency.NewPostgresStore(a.pool)
	} else {
		a.keys = idempotency.NewMemoryStore()
	}
	a.keysPurge = idempotency.NewPurger(a.keys, cfg.Idempotency, a.log.Logger)
}

// newGuards wires the inbound protection: who the caller is, and how much of
// the service they may have.
//
// The resolver is built in every mode, including auth.mode=off, because the
// limiter keys on the identity it produces. Skipping it there would leave every
// anonymous caller sharing one bucket, which is worse than no limiter at all.
func (a *App) newGuards(cfg *config.Config) {
	a.auth = auth.NewResolver(cfg.Auth, cfg.HTTP.TrustedProxyHops)

	if cfg.RateLimit.Enabled {
		a.limiter = ratelimit.New(cfg.RateLimit, a.log.Logger, a.metrics.Guards)
	}
}

// inflightLimit is the load-shedding cap, or zero when the limiter is off.
func inflightLimit(cfg *config.Config) int {
	if !cfg.RateLimit.Enabled {
		return 0
	}
	return cfg.RateLimit.GlobalInflight
}
