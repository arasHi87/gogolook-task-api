package ratelimit

import (
	"log/slog"
	"net/http"

	"github.com/arasHi87/gogolook-task-api/internal/httpx"
	"github.com/arasHi87/gogolook-task-api/internal/logging"
)

// Inflight caps concurrent requests, shedding the rest.
//
// This is the tier that actually keeps the process alive, and it is a
// different question from the one the token bucket answers. A rate limiter
// bounds arrivals; it says nothing about how many are still running. Two
// hundred arrivals per second of ten-second requests is two thousand
// concurrent goroutines, each holding a connection, a buffer and possibly a
// database connection — and the limiter was configured correctly the whole
// time.
//
// A semaphore in middleware rather than netutil.LimitListener at the
// connection level, because a refused connection is a client-side error with
// no explanation, where this can return 503 with Retry-After and say which
// limit was hit.
//
// Shedding immediately rather than queueing is deliberate. A queue in front of
// an overloaded server converts a fast failure into a slow one: the caller
// waits, times out, retries, and the work already done is thrown away. Load
// shedding is what keeps the requests that *are* being served fast.
func Inflight(limit int) httpx.Middleware {
	// A buffered channel is the semaphore. Its length is the number in flight,
	// which is also how it stays lock-free.
	slots := make(chan struct{}, limit)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			default:
				logging.From(r.Context()).Warn("request shed: too many in flight",
					slog.Int("limit", limit))

				w.Header().Set("Retry-After", "1")
				httpx.WriteProblem(w, r, http.StatusServiceUnavailable,
					"too many requests in flight; retry shortly")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
