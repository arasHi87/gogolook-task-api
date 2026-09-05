package httpx

import (
	"net/http"
	"strconv"
)

// MaxBody caps how much a client may send.
//
// http.MaxBytesReader is what makes this real: it is enforced as the body is
// read, so a request that lies about Content-Length is still cut off, and the
// reader fails rather than the process filling memory. The Content-Length check
// in front of it is only an early exit that saves reading the body at all.
func MaxBody(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limit <= 0 {
				next.ServeHTTP(w, r)
				return
			}

			if r.ContentLength > limit {
				w.Header().Set("Connection", "close")
				writeProblem(w, r, http.StatusRequestEntityTooLarge,
					"request body exceeds the limit of "+strconv.FormatInt(limit, 10)+" bytes")
				return
			}

			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}
