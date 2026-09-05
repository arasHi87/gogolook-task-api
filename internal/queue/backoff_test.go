package queue_test

import (
	"math"
	"testing"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/queue"
)

func backoffConfig(base, max time.Duration, jitter string) config.Backoff {
	return config.Backoff{
		Base:   config.Duration(base),
		Max:    config.Duration(max),
		Jitter: jitter,
	}
}

// attempt^4, not 2^n. Exponential is too aggressive early, where a failure is
// usually a blip worth retrying soon, and too short at the tail, where a
// dependency outage needs minutes.
func TestBackoffCurve(t *testing.T) {
	t.Parallel()

	b := queue.NewBackoff(backoffConfig(time.Second, time.Hour, config.JitterNone))

	want := map[int]time.Duration{
		1: 1 * time.Second,
		2: 16 * time.Second,
		3: 81 * time.Second,
		4: 256 * time.Second,
		5: 625 * time.Second,
	}
	for attempt, expected := range want {
		if got := b.For(attempt); got != expected {
			t.Errorf("attempt %d: %v, want %v", attempt, got, expected)
		}
	}
}

func TestBackoffIsCapped(t *testing.T) {
	t.Parallel()

	const cap = 5 * time.Minute
	b := queue.NewBackoff(backoffConfig(time.Second, cap, config.JitterNone))

	for _, attempt := range []int{10, 100, 10_000, math.MaxInt32} {
		if got := b.For(attempt); got != cap {
			t.Errorf("attempt %d: %v, want the cap %v", attempt, got, cap)
		}
	}
}

// Jitter is what stops every replica retrying in lockstep. Without it a
// dependency that recovers is hit at once by every job that failed against it,
// which is how a recovering service goes back down.
func TestJitterSpreadsRetries(t *testing.T) {
	t.Parallel()

	const attempt = 3
	raw := 81 * time.Second

	t.Run("full jitter covers the whole window", func(t *testing.T) {
		t.Parallel()
		b := queue.NewBackoff(backoffConfig(time.Second, time.Hour, config.JitterFull))

		var lo, hi time.Duration = raw, 0
		for range 2000 {
			d := b.For(attempt)
			if d < 0 || d > raw {
				t.Fatalf("delay %v is outside [0, %v]", d, raw)
			}
			lo, hi = min(lo, d), max(hi, d)
		}
		// Full jitter draws from the whole window, so a large sample should
		// reach near both ends.
		if lo > raw/10 {
			t.Errorf("smallest of 2000 draws was %v; full jitter should reach near zero", lo)
		}
		if hi < raw*9/10 {
			t.Errorf("largest of 2000 draws was %v; full jitter should reach near %v", hi, raw)
		}
	})

	t.Run("equal jitter never returns near zero", func(t *testing.T) {
		t.Parallel()
		b := queue.NewBackoff(backoffConfig(time.Second, time.Hour, config.JitterEqual))

		for range 2000 {
			d := b.For(attempt)
			if d < raw/2 || d > raw {
				t.Fatalf("delay %v is outside [%v, %v]", d, raw/2, raw)
			}
		}
	})

	t.Run("no jitter is exact", func(t *testing.T) {
		t.Parallel()
		b := queue.NewBackoff(backoffConfig(time.Second, time.Hour, config.JitterNone))

		for range 100 {
			if got := b.For(attempt); got != raw {
				t.Fatalf("delay %v, want exactly %v", got, raw)
			}
		}
	})
}

// Attempts are 1-based, and a nonsensical attempt must not produce a negative
// or zero delay that would make a failing job spin.
func TestBackoffFloor(t *testing.T) {
	t.Parallel()

	b := queue.NewBackoff(backoffConfig(time.Second, time.Hour, config.JitterNone))
	for _, attempt := range []int{0, -1, -1000} {
		if got := b.For(attempt); got != time.Second {
			t.Errorf("attempt %d: %v, want the attempt-1 delay", attempt, got)
		}
	}
}
