// Package idempotency is the producer half of at-least-once.
//
// The queue guarantees an event is delivered at least once. This guarantees a
// request that is *sent* more than once is *executed* once. They are the two
// ends of the same problem: a client that times out cannot tell a request that
// failed from one that succeeded and lost its response, so it retries, and
// without a key on the wire the server cannot tell either.
//
// The mechanism is the one described in
// draft-ietf-httpapi-idempotency-key-header, which is itself what Stripe and
// PayPal do:
//
//	Idempotency-Key: <opaque>   on a write
//	  first time              → execute, remember the response
//	  again, same request     → replay the stored response verbatim
//	  again, different request→ 422; the client reused a key it should not have
//	  again, still running    → 409; the first one has not finished
//
// The part that is easy to get wrong is not the table, it is the transaction.
// The stored response and the write that produced it commit together, in one
// transaction opened here and joined by the repository. Two transactions
// cannot do it: whichever commits second can fail, and then either a key
// promises a response for a task that does not exist, or a task exists whose
// key is stuck half-written and whose retry is refused until the TTL expires.
package idempotency

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"unicode"
)

// HeaderKey is the request header carrying the key.
const HeaderKey = "Idempotency-Key"

// HeaderReplayed marks a response served from the store rather than executed.
// It is not in the draft, but Stripe sends the equivalent and it is the
// difference between a client that can verify its retry logic and one that
// cannot.
const HeaderReplayed = "Idempotency-Replayed"

// Fingerprint identifies the request a key was used for.
//
// Method, canonical path and body, hashed. It is what separates a retry from a
// mistake: the same key with the same fingerprint is the client asking again
// for an answer it did not hear, and the same key with a different fingerprint
// is a client reusing a key — a bug that would otherwise return one request's
// response to a different request.
//
// The parts are length-prefixed rather than concatenated, so no combination of
// a path and a body can be re-cut into a different path and body that hash the
// same.
func Fingerprint(method, path string, body []byte) []byte {
	h := sha256.New()
	for _, part := range [][]byte{[]byte(method), []byte(canonicalPath(path)), body} {
		_, _ = fmt.Fprintf(h, "%d:", len(part))
		_, _ = h.Write(part)
	}
	return h.Sum(nil)
}

// canonicalPath collapses the two surfaces onto one.
//
// /api/v1/tasks and /tasks are the same endpoint served twice, so a key used
// on one and retried on the other is a retry, not key reuse. Without this the
// second request would be told 422 for doing exactly what the contract invites
// it to do.
func canonicalPath(path string) string {
	if rest, ok := strings.CutPrefix(path, "/api/v1/"); ok {
		return "/" + rest
	}
	if path == "/api/v1" {
		return "/"
	}
	return path
}

// CheckKey validates what the client sent.
//
// The key is a primary key in a table we own, so it is bounded, and it is
// echoed into logs and error bodies, so it is printable ASCII. The draft says
// only that the key is an opaque string; leaving it unconstrained means a
// client can write arbitrary bytes into our index.
func CheckKey(key string, maxBytes int) error {
	switch {
	case key == "":
		return fmt.Errorf("%s must not be empty", HeaderKey)
	case len(key) > maxBytes:
		return fmt.Errorf("%s must be at most %d bytes, got %d", HeaderKey, maxBytes, len(key))
	}

	for _, r := range key {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			return fmt.Errorf("%s must be printable ASCII", HeaderKey)
		}
	}
	return nil
}
