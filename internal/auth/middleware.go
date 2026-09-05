package auth

import (
	"log/slog"
	"net/http"

	"github.com/arasHi87/gogolook-task-api/internal/httpx"
	"github.com/arasHi87/gogolook-task-api/internal/logging"
)

// Middleware resolves the caller and puts the identity in the context.
//
// It runs early — before the rate limiter, which needs the identity to pick a
// quota, and before the logger's outcome line, so every record carries the
// client id.
//
// In optional mode it never rejects anything. In required mode an
// unrecognised caller gets 401 with the challenge RFC 9110 requires: a 401
// without WWW-Authenticate tells the client it needs credentials but not what
// kind, which is how a working client and a broken one look the same.
func Middleware(r *Resolver, obs Observer) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			id, ok := r.Resolve(req)
			if obs != nil {
				obs.Authenticated(outcome(id, ok))
			}
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.Realm()+`"`)
				httpx.WriteProblem(w, req, http.StatusUnauthorized,
					"a valid bearer token is required")
				return
			}

			ctx := Into(req.Context(), id)
			// The client id, never the token and never the key: the key is the
			// caller's IP for anonymous traffic, and an IP in a log field that
			// feeds a dashboard is the same cardinality problem as an IP in a
			// label.
			ctx = logging.Into(ctx, logging.From(ctx).With(
				slog.String("client_id", id.ClientID),
				slog.String("tier", id.Tier),
			))
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
}

// Observer receives resolution outcomes, for metrics. Declared here and
// satisfied elsewhere, so this package never imports the metrics registry.
type Observer interface {
	Authenticated(result string)
}

// outcome names how the caller was resolved, in three bounded values.
func outcome(id Identity, ok bool) string {
	switch {
	case !ok:
		return "rejected"
	case id.IsAnonymous():
		return "anonymous"
	default:
		return "authenticated"
	}
}

// The middleware runs in every mode, including off. Skipping it there would
// leave the identity absent from the context, and the rate limiter would then
// key every anonymous caller on the same empty string — one shared bucket for
// the whole internet, which is worse than no limiter at all.
