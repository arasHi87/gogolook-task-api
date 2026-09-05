package queue

import (
	"math"
	"math/rand/v2"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

// Backoff computes how long to wait before attempt n is tried again.
//
// The curve is base * attempt^4, which with a one-second base gives roughly
// 1s, 16s, 81s, 256s, 625s. That is deliberately not 2^n: exponential is too
// aggressive early, where a failure is usually a blip worth retrying soon, and
// too short at the tail, where a dependency outage needs minutes rather than
// seconds.
//
// Jitter is what stops every replica retrying in lockstep. Without it, a
// dependency that recovers is immediately hit by every job that failed against
// it at the same instant — which is how a recovering service goes back down.
type Backoff struct {
	Base   time.Duration
	Max    time.Duration
	Jitter string
	// rand is injectable so a test can assert the bounds of the window rather
	// than the value drawn from it.
	rand func() float64
}

// NewBackoff builds a backoff from configuration.
func NewBackoff(cfg config.Backoff) Backoff {
	return Backoff{
		Base:   cfg.Base.D(),
		Max:    cfg.Max.D(),
		Jitter: cfg.Jitter,
		rand:   rand.Float64,
	}
}

// For returns the delay before the given attempt is retried. Attempts are
// 1-based: attempt 1 has just failed, and this is the wait before attempt 2.
func (b Backoff) For(attempt int) time.Duration {
	raw := b.raw(attempt)

	random := b.rand
	if random == nil {
		random = rand.Float64
	}

	switch b.Jitter {
	case config.JitterNone:
		return raw

	case config.JitterEqual:
		// Half fixed, half random: never returns near-zero, so a retry storm
		// still spreads but nothing retries immediately.
		half := raw / 2
		return half + time.Duration(random()*float64(half))

	default:
		// Full jitter, which is what AWS recommends: sleep = rand(0, window).
		// Equal jitter and no jitter both let replicas reconverge on the same
		// schedule after a shared outage; full jitter does not.
		return time.Duration(random() * float64(raw))
	}
}

// raw is the un-jittered window for an attempt, capped.
func (b Backoff) raw(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	// float64 rather than integer arithmetic: attempt^4 on a large attempt
	// count overflows int64 well before the cap would have applied.
	window := float64(b.Base) * math.Pow(float64(attempt), 4)
	if window > float64(b.Max) || math.IsInf(window, 1) {
		return b.Max
	}
	return time.Duration(window)
}
