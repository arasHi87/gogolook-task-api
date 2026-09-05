package httpx

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/logging"
)

// Logger installs a request-scoped logger and emits one canonical log line per
// request.
//
// One wide record rather than five scattered ones: everything known about the
// request lands in a single structured event, so a query answers a question
// instead of requiring a join across lines.
//
// It is DEBUG, not INFO, and that is deliberate. At a thousand requests a
// second an INFO request log is a thousand lines a second of noise burying the
// handful of lines that describe an actual state change. Requests are a metric;
// the log is for narrative and for the -v case where someone is watching.
func Logger() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := logging.With(r.Context(),
				slog.String("request_id", RequestIDFrom(r.Context())),
			)
			r = r.WithContext(ctx)

			log := logging.From(ctx)
			if !log.Enabled(ctx, slog.LevelDebug) {
				// Skip the recorder entirely when nobody is listening: it costs
				// an allocation and a wrapper on every response write.
				next.ServeHTTP(w, r)
				return
			}

			// Templated before the handler runs, not after. The transcoder
			// rewrites r.URL.Path in place to the RPC procedure it dispatched
			// to, so reading the path afterwards labels every REST request as
			// the same Connect route — which makes the log useless and would
			// make a route metric label lie.
			route := Route(r.URL.Path)

			rec := &recorder{ResponseWriter: w, status: http.StatusOK}
			started := time.Now()
			next.ServeHTTP(rec, r)

			log.LogAttrs(ctx, slog.LevelDebug, "request",
				slog.String("method", r.Method),
				slog.String("route", route),
				slog.Int("status", rec.status),
				slog.Float64("dur_ms", float64(time.Since(started).Microseconds())/1000),
				slog.Int64("bytes_in", r.ContentLength),
				slog.Int64("bytes_out", rec.written),
			)
		})
	}
}

// recorder captures the status and byte count for the canonical log line.
type recorder struct {
	http.ResponseWriter
	status  int
	written int64
	wrote   bool
}

func (r *recorder) WriteHeader(status int) {
	if r.wrote {
		return
	}
	r.wrote = true
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.wrote = true
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer, so flushing
// and deadline control still work through the wrapper.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
