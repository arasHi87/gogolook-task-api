package httpx

import (
	"encoding/json"
	"net/http"
)

// ProblemContentType is RFC 9457's media type for machine-readable errors.
const ProblemContentType = "application/problem+json"

// problem is an RFC 9457 problem document, trimmed to the fields that carry
// information here.
type problem struct {
	// Type is a URI identifying the problem class. "about:blank" is the
	// standard's own value for "the status code says it all".
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Instance carries the request id, which is what turns a user's screenshot
	// into a log query.
	Instance string `json:"instance,omitempty"`
}

// writeProblem emits an error the middleware chain generates itself — before a
// request reaches the transcoder, or after it panicked out of one.
//
// Errors returned by a handler take Connect's own error path instead, so that
// gRPC and Connect clients get a native error rather than a JSON body they
// cannot interpret. Both shapes carry the request id.
func writeProblem(w http.ResponseWriter, r *http.Request, status int, detail string) {
	// Anything already written by the handler makes this a no-op with a broken
	// body; better to stop than to append garbage.
	if w.Header().Get("Content-Type") != "" && status == http.StatusInternalServerError {
		return
	}

	w.Header().Set("Content-Type", ProblemContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(problem{
		Type:     "about:blank",
		Title:    http.StatusText(status),
		Status:   status,
		Detail:   detail,
		Instance: RequestIDFrom(r.Context()),
	})
}
