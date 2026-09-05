package httpx

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/arasHi87/gogolook-task-api/internal/logging"
)

// Recover turns a panic in a handler into a 500 and a log line with a stack.
//
// Without it, net/http's own recovery closes the connection with no response
// and no attribution, so the client sees a transport error and the operator
// sees nothing they can act on.
func Recover() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			//nolint:contextcheck // the deferred closure uses r.Context(); the
			// linter cannot see through the defer.
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// A client that goes away mid-write makes net/http panic with
				// this sentinel. It is not our bug and it is not worth a stack.
				if errors.Is(errorOf(rec), http.ErrAbortHandler) {
					panic(rec)
				}

				logging.From(r.Context()).Error("panic recovered",
					slog.Any("panic", rec),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("stack", string(debug.Stack())),
				)

				writeProblem(w, r, http.StatusInternalServerError, "internal error")
			}()

			next.ServeHTTP(w, r)
		})
	}
}

func errorOf(v any) error {
	if err, ok := v.(error); ok {
		return err
	}
	return nil
}
