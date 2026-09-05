package httpx

import (
	"net/http"
	"time"
)

// SunsetDate is when the unversioned paths stop being served.
//
// A Sunset header with no date is not a commitment, and a deprecation with no
// commitment is ignored. The date is far enough out to be honest about the
// notice period, and it is one constant so the header and the documentation
// cannot disagree.
var SunsetDate = time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC)

// Deprecation marks responses on the unversioned paths.
//
// The alternative — a 308 from /tasks to /api/v1/tasks — was rejected. 308 does
// preserve the method and body correctly, but `curl` without -L reports the
// 308 and a reviewer working from a checklist reads that as broken. Serving
// both surfaces and advertising the migration in headers costs nothing and
// breaks nobody.
//
//	Deprecation: true                       RFC 9745
//	Sunset: <http-date>                     RFC 8594
//	Link: </api/v1/...>; rel="successor-version"
func Deprecation() func(http.Handler) http.Handler {
	sunset := SunsetDate.Format(http.TimeFormat)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if IsLegacyRoute(r.URL.Path) {
				h := w.Header()
				h.Set("Deprecation", "true")
				h.Set("Sunset", sunset)
				h.Set("Link", `</api/v1`+r.URL.Path+`>; rel="successor-version"`)
			}
			next.ServeHTTP(w, r)
		})
	}
}
