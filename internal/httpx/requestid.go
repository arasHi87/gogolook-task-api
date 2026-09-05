// Package httpx holds the HTTP middleware chain.
//
// Each middleware here is a plain func(http.Handler) http.Handler, so the chain
// is readable top to bottom at the one place it is assembled and there is no
// framework to learn.
package httpx

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// RequestIDHeader is both read and written: an id supplied by an upstream proxy
// is preserved so one identifier follows a request across services.
const RequestIDHeader = "X-Request-Id"

// maxInboundRequestID bounds what we will echo back. An unbounded header value
// reflected into every log line and response is a free amplification primitive.
const maxInboundRequestID = 64

type requestIDKey struct{}

// RequestID attaches a request id to the context and the response.
func RequestID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := sanitizeRequestID(r.Header.Get(RequestIDHeader))
			if id == "" {
				id = uuid.NewString()
			}

			w.Header().Set(RequestIDHeader, id)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
		})
	}
}

// RequestIDFrom returns the id attached to ctx, or "".
func RequestIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// sanitizeRequestID keeps an inbound id only if it is short and printable
// ASCII. Anything else is discarded and replaced, because this value ends up in
// a response header and in structured logs.
func sanitizeRequestID(raw string) string {
	if raw == "" || len(raw) > maxInboundRequestID {
		return ""
	}
	for i := range len(raw) {
		if c := raw[i]; c < 0x20 || c > 0x7e {
			return ""
		}
	}
	return raw
}
