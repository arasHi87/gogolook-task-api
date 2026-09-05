package idempotency

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/httpx"
	"github.com/arasHi87/gogolook-task-api/internal/logging"
)

// Middleware applies Idempotency-Key to unsafe requests.
//
// It sits innermost in the chain, closest to the handler, for two reasons: the
// body it buffers must already be size-capped by MaxBody, and the response it
// stores must be the one the handler produced rather than one a later
// middleware has decorated.
//
// A safe method passes straight through. So does an unsafe one with no key,
// unless the configuration says a key is required — a service that rejects the
// assignment's own contract for not sending a header the assignment never
// mentions is not more correct, it is broken.
func Middleware(store Store, cfg config.Idempotency) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !cfg.Enabled || safeMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}

			key := r.Header.Get(HeaderKey)
			if key == "" {
				if cfg.Required {
					httpx.WriteProblem(w, r, http.StatusBadRequest,
						HeaderKey+" is required on "+r.Method)
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			if err := CheckKey(key, cfg.MaxKeyBytes); err != nil {
				httpx.WriteProblem(w, r, http.StatusBadRequest, err.Error())
				return
			}
			handle(w, r, next, store, cfg, key)
		})
	}
}

// handle is the part that only runs for a keyed write, split out so the guard
// clauses above read as a list of "not our business" rather than as nesting.
func handle(
	w http.ResponseWriter,
	r *http.Request,
	next http.Handler,
	store Store,
	cfg config.Idempotency,
	key string,
) {
	body, err := readBody(r)
	if err != nil {
		// The only way this fails is the size cap, which MaxBody sitting
		// outside us has already decided is a 413.
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpx.WriteProblem(w, r, http.StatusRequestEntityTooLarge, "request body is too large")
			return
		}
		httpx.WriteProblem(w, r, http.StatusBadRequest, "could not read the request body")
		return
	}

	fingerprint := Fingerprint(r.Method, r.URL.Path, body)

	unit, seen, err := store.Reserve(r.Context(), key, fingerprint, cfg.TTL.D())
	if err != nil {
		logging.From(r.Context()).Error("idempotency store unavailable", slog.Any("err", err))
		httpx.WriteProblem(w, r, http.StatusServiceUnavailable,
			"could not record the idempotency key; retry with the same key")
		return
	}
	if seen != nil {
		replay(w, r, seen, fingerprint)
		return
	}
	execute(w, r, next, unit, key)
}

// execute runs the handler inside the reserved unit and stores what it
// produced.
func execute(w http.ResponseWriter, r *http.Request, next http.Handler, unit Unit, key string) {
	defer unit.Abandon() // no-op once Complete has run

	// Buffered, not streamed. The response has to be stored before it is sent:
	// a client that receives a 200 and then finds the key was never completed
	// would retry and execute the write a second time.
	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	// unit.Context() is r.Context() with the store's transaction attached, not
	// a new root — the linter cannot see that through the interface.
	//nolint:contextcheck // derived from r.Context(); see Unit.Context
	next.ServeHTTP(rec, identityRequest(unit.Context(), r))

	if !stored(rec.status) {
		// The request failed, so the key is released rather than remembered.
		// Remembering it would refuse the client a retry of something that
		// never happened — and everything the handler wrote rolls back with
		// it, so there is nothing left to be inconsistent with.
		unit.Abandon()
		rec.flush(w)
		return
	}

	if err := unit.Complete(rec.status, rec.header.Get("Content-Type"), rec.body.Bytes()); err != nil {
		// Nothing committed, so nothing happened: no task, no event, no key.
		// Telling the client it succeeded would be a lie, and 503 with the
		// same key is a retry that will work.
		logging.From(r.Context()).Error("idempotent write could not commit",
			slog.String("idempotency_key", key), slog.Any("err", err))
		httpx.WriteProblem(w, r, http.StatusServiceUnavailable,
			"the write could not be committed; retry with the same key")
		return
	}
	rec.flush(w)
}

// replay answers a key that has been used before.
func replay(w http.ResponseWriter, r *http.Request, seen *Record, fingerprint []byte) {
	// Constant time, because the fingerprint is derived from the request body
	// and a timing oracle on it leaks what another client sent.
	if subtle.ConstantTimeCompare(seen.Fingerprint, fingerprint) != 1 {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity,
			HeaderKey+" has already been used for a different request")
		return
	}

	if !seen.Completed() {
		// Still running, somewhere. 409 rather than blocking: the client knows
		// how long it is willing to wait for its own request and we do not,
		// and a request parked here holds a connection on both sides.
		w.Header().Set("Retry-After", "1")
		httpx.WriteProblem(w, r, http.StatusConflict,
			"a request with this "+HeaderKey+" is still in progress")
		return
	}

	h := w.Header()
	if seen.ContentType != "" {
		h.Set("Content-Type", seen.ContentType)
	}
	// These bytes were produced by our own handler and stored, but they are
	// being written back with a content type that came out of the same row.
	// nosniff stops a browser deciding for itself that the payload is HTML,
	// which is the one way a stored response could become an injection.
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set(HeaderReplayed, "true")
	h.Set("Content-Length", strconv.Itoa(len(seen.Body)))
	w.WriteHeader(seen.StatusCode)
	//nolint:gosec // the body is this service's own stored response, served with nosniff
	_, _ = w.Write(seen.Body)
}

// stored reports whether a response is worth remembering.
//
// Only success. A 4xx is the client's to fix and a 5xx is ours, and replaying
// either one to a retry would turn a transient failure into a permanent one:
// the client would keep being told about a problem that may already be gone.
func stored(status int) bool { return status >= 200 && status < 300 }

// safeMethod reports whether a method has no effect worth deduplicating.
func safeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// identityRequest asks the handler for an uncompressed response.
//
// What gets stored has to be servable to *any* later retry, and a compressed
// body is only servable to a client that accepts that encoding. The handler
// compresses according to this request's Accept-Encoding, and the middleware
// sits outside it, so what would otherwise be captured is the encoded form —
// replayed later to a client that may not have asked for it, or replayed
// without its Content-Encoding and read as garbage. Both were observed before
// this existed.
//
// So the encoding is negotiated away for keyed writes: the stored bytes are
// the canonical representation, and every retry can be answered from them. The
// cost is that a keyed write is not compressed, which for a single task object
// is a few hundred bytes that gzip would mostly have made larger.
//
// Clone rather than mutate: the header map is shared with the request the
// outer middleware is still holding.
func identityRequest(ctx context.Context, r *http.Request) *http.Request {
	inner := r.Clone(ctx)
	inner.Header.Del("Accept-Encoding")
	return inner
}

// readBody consumes the body and puts it back, so the handler still sees it.
func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}
