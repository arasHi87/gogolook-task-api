// Package ratelimit protects the service from its callers.
//
// It points the opposite way from a circuit breaker, and the two are not
// substitutes: a rate limiter guards against callers, a breaker guards against
// dependencies. A breaker on an inbound handler and a limiter used as an
// outbound failure guard are both common and both wrong.
//
// There are two tiers here, and only one of them keeps the process alive:
//
//   - The in-flight limiter is a counting semaphore. It is what actually
//     survives overload, because 200 requests per second of ten-second
//     requests is still two thousand concurrent goroutines, and a rate limiter
//     has nothing to say about that.
//   - The per-client limiter is a token bucket per caller. It is about
//     fairness — one client must not be able to consume everyone's capacity —
//     and it is the reason the quota is per tier rather than one global number.
//
// The honest caveat, which belongs in the README as much as here: an
// in-process limiter is per replica. Three replicas behind a load balancer
// enforce three times the nominal limit. Real deployments enforce this at the
// edge with a shared counter; this is the backstop for when the edge is
// misconfigured, not the primary control.
package ratelimit

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/arasHi87/gogolook-task-api/internal/auth"
	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/logging"
)

// Limiter is one token bucket per caller, evicted when the caller goes away.
//
// The eviction is not housekeeping, it is the whole design. A plain map keyed
// by caller is an unbounded allocation driven by attacker input — the bug in
// most published implementations — because every new IP mints a bucket that is
// never freed.
type Limiter struct {
	cfg config.RateLimit
	log *slog.Logger

	mu      sync.Mutex
	buckets map[string]*bucket

	// now is a seam for the tests. Sweeping is time-based, and a test that has
	// to sleep for ten minutes is a test nobody runs.
	now func() time.Time
}

type bucket struct {
	limiter *rate.Limiter
	tier    string
	seen    time.Time
}

// New builds a limiter from configuration.
func New(cfg config.RateLimit, log *slog.Logger) *Limiter {
	if log == nil {
		log = slog.Default()
	}
	return &Limiter{
		cfg:     cfg,
		log:     log.With(slog.String("component", "ratelimit")),
		buckets: map[string]*bucket{},
		now:     time.Now,
	}
}

// Decision is the outcome of one check, and everything the response headers
// need. It is returned for allowed requests too: a client that is told its
// remaining quota can pace itself, where one that only learns at the wall
// discovers the limit by hitting it.
type Decision struct {
	Allowed bool
	// Limit and Window describe the policy in force, which is the tier's, not
	// a single global number.
	//
	// Limit is the bucket's burst and Window is how long that burst takes to
	// refill, rather than the sustained rate over one second. The two describe
	// the same quota — burst over window is the rate — but only this framing
	// keeps every field simultaneously true. Reporting "10 per second" while
	// the bucket holds 20 tokens means Remaining exceeds Limit whenever the
	// caller has been quiet, which is a header set no client can act on.
	Limit  int
	Window time.Duration
	// Remaining is whole tokens left in the bucket.
	Remaining int
	// Reset is how long until the next token, zero when one is available.
	Reset time.Duration
	Tier  string
}

// Allow takes a token for the caller and reports what happened.
func (l *Limiter) Allow(id auth.Identity) Decision {
	quota := l.quota(id.Tier)

	l.mu.Lock()
	b, ok := l.buckets[id.Key]
	if !ok || b.tier != id.Tier {
		// A changed tier means the same key was seen with a different token.
		// Reusing the old bucket would apply the old quota until it expired.
		b = &bucket{
			limiter: rate.NewLimiter(rate.Limit(quota.Rate), quota.Burst),
			tier:    id.Tier,
		}
		l.buckets[id.Key] = b
	}
	b.seen = l.now()
	l.mu.Unlock()

	// Outside the lock: rate.Limiter has its own, and holding both would make
	// the map's mutex the contention point for every request in the process.
	now := l.now()
	allowed := b.limiter.AllowN(now, 1)

	d := Decision{
		Allowed: allowed,
		Limit:   quota.Burst,
		Window:  refill(quota),
		Tier:    id.Tier,
	}
	d.Remaining = max(int(b.limiter.TokensAt(now)), 0)
	if !allowed {
		// Reserve to ask when a token will exist, then cancel it: the answer
		// is wanted, the token is not, and leaving it taken would charge the
		// caller for a request that was refused.
		r := b.limiter.ReserveN(now, 1)
		d.Reset = r.DelayFrom(now)
		r.CancelAt(now)
	}
	return d
}

// refill is how long a full burst takes to accumulate at the sustained rate.
//
// Rounded up to whole seconds, because the header carries an integer. Rounding
// up advertises a slightly lower rate than the limiter enforces, which is the
// right direction to be wrong in: a client that paces itself to it stays under
// the real limit.
func refill(q config.Quota) time.Duration {
	if q.Rate <= 0 {
		return time.Second
	}
	seconds := math.Ceil(float64(q.Burst) / q.Rate)
	return time.Duration(max(seconds, 1)) * time.Second
}

// quota selects the tier's settings. An unknown tier gets the strictest one:
// failing to the smallest quota means a configuration mistake shows up as
// throttling rather than as an unlimited caller.
func (l *Limiter) quota(tier string) config.Quota {
	switch tier {
	case config.TierInternal:
		return l.cfg.Tiers.Internal
	case config.TierStandard:
		return l.cfg.Tiers.Standard
	default:
		return l.cfg.Tiers.Anonymous
	}
}

// Len is how many buckets are held. Tests assert on it; nothing else reads it.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Run sweeps idle buckets until ctx is cancelled.
func (l *Limiter) Run(ctx context.Context) error {
	ctx = logging.Into(ctx, l.log)

	tick := time.NewTicker(l.cfg.SweepInterval.D())
	defer tick.Stop()

	l.log.Info("rate limiter started",
		slog.Duration("key_ttl", l.cfg.KeyTTL.D()),
		slog.Duration("sweep", l.cfg.SweepInterval.D()),
		slog.Int("global_inflight", l.cfg.GlobalInflight))

	for {
		select {
		case <-ctx.Done():
			l.log.Info("rate limiter stopped")
			return nil
		case <-tick.C:
			if n := l.Sweep(); n > 0 {
				logging.Trace(ctx, "idle rate-limit buckets evicted", slog.Int("count", n))
			}
		}
	}
}

// Sweep drops buckets nobody has used for the TTL, and returns how many went.
//
// A bucket that has been idle for longer than the TTL has necessarily refilled
// to full, so dropping it and building a fresh one on the next request is
// exactly equivalent — there is no quota to lose.
func (l *Limiter) Sweep() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := l.now().Add(-l.cfg.KeyTTL.D())
	n := 0
	for key, b := range l.buckets {
		if b.seen.Before(cutoff) {
			delete(l.buckets, key)
			n++
		}
	}
	return n
}
