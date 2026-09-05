package idempotency

import (
	"bytes"
	"net/http"
)

// recorder captures a response instead of sending it.
//
// It exists because the response has to be stored before it is sent. Streaming
// it and storing afterwards leaves a window in which the client has been told
// the write succeeded while the key that makes the write idempotent was never
// written — and the client's retry then executes it again.
//
// The responses this wraps are one task object. Buffering something unbounded
// would be a different decision.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
	// wrote guards against a handler calling WriteHeader twice, which
	// net/http would log as an error and which would otherwise let the second
	// status win here while the first won on the wire.
	wrote bool
}

// Header implements http.ResponseWriter.
func (r *recorder) Header() http.Header { return r.header }

// WriteHeader implements http.ResponseWriter.
func (r *recorder) WriteHeader(status int) {
	if r.wrote {
		return
	}
	r.wrote = true
	r.status = status
}

// Write implements http.ResponseWriter.
func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.body.Write(b)
}

// Flush implements http.Flusher, as a deliberate no-op.
//
// connect-go type-asserts the ResponseWriter for this and refuses to serve
// without it, because a streaming RPC has to be able to push a message before
// the call ends. Nothing here streams: the transcoded REST endpoints are all
// unary, and the response is buffered on purpose so it can be stored before it
// is sent. Passing the flush through to the real writer would defeat that by
// sending the headers early, which is why this does nothing rather than
// forwarding.
//
// The consequence, stated rather than discovered: a streaming handler behind
// this middleware would be buffered to completion. Idempotency-Key is defined
// for unary writes, and the middleware only wraps unsafe methods, so no
// streaming call reaches it today.
func (r *recorder) Flush() {}

// Unwrap is deliberately not implemented. http.ResponseController would use it
// to reach the writer underneath, which is exactly what must not happen while
// a response is being captured.

// flush sends the captured response.
func (r *recorder) flush(w http.ResponseWriter) {
	dst := w.Header()
	for name, values := range r.header {
		dst[name] = values
	}
	w.WriteHeader(r.status)
	_, _ = w.Write(r.body.Bytes())
}
