package ratelimit

import (
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/auth"
	"github.com/arasHi87/gogolook-task-api/internal/httpx"
	"github.com/arasHi87/gogolook-task-api/internal/logging"
)

// Middleware applies the per-caller quota.
//
// It runs after the auth middleware, which is what puts the identity — and
// therefore the tier and the bucket key — in the context.
func Middleware(l *Limiter) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := auth.From(r.Context())
			d := l.Allow(id)
			writeHeaders(w.Header(), d)

			if l.obs != nil {
				l.obs.RateLimited(d.Tier, d.Allowed)
			}

			if d.Allowed {
				next.ServeHTTP(w, r)
				return
			}

			// One line per shed request would be a flood exactly when the log
			// is least readable. The message is a constant, so the throttling
			// handler collapses the repeats to one per window carrying a
			// suppressed count — the first refusal is still immediate.
			logging.From(r.Context()).Warn("request shed by the rate limiter",
				slog.String("tier", id.Tier),
				slog.Int("limit", d.Limit))

			w.Header().Set("Retry-After", retryAfter(d.Reset))
			httpx.WriteProblem(w, r, http.StatusTooManyRequests,
				"rate limit exceeded for the "+id.Tier+" tier")
		})
	}
}

// writeHeaders publishes the quota, on every response and not just the refused
// ones.
//
// Both spellings, on purpose. The IETF draft collapsed to RateLimit plus
// RateLimit-Policy; the older Limit/Remaining/Reset trio is still what GitHub,
// Stripe and most SDKs actually read. Emitting only the new one is correct and
// useless; emitting only the old one is useful and dated.
func writeHeaders(h http.Header, d Decision) {
	window := int(math.Round(d.Window.Seconds()))
	reset := int(math.Ceil(d.Reset.Seconds()))

	policy := strconv.Itoa(d.Limit) + ";w=" + strconv.Itoa(window)
	h.Set("RateLimit-Policy", policy)
	h.Set("RateLimit", "limit="+strconv.Itoa(d.Limit)+
		", remaining="+strconv.Itoa(d.Remaining)+
		", reset="+strconv.Itoa(reset))

	h.Set("RateLimit-Limit", strconv.Itoa(d.Limit))
	h.Set("RateLimit-Remaining", strconv.Itoa(d.Remaining))
	h.Set("RateLimit-Reset", strconv.Itoa(reset))
}

// retryAfter renders the delay as the integer seconds the header requires.
//
// Rounded up and floored at one: a Retry-After of 0 invites an immediate retry,
// which is the request that was just refused.
func retryAfter(d time.Duration) string {
	return strconv.Itoa(max(int(math.Ceil(d.Seconds())), 1))
}
