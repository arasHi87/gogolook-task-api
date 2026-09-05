package ratelimit

import "time"

// SetClock replaces the limiter's clock.
//
// Exported to the test package only. Eviction is time-based, and a test that
// has to sleep for the ten-minute TTL is a test nobody runs — so the clock is
// a seam rather than the thing under test.
func SetClock(l *Limiter, now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}
