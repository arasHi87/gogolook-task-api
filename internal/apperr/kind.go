// Package apperr is the shared vocabulary for classifying errors.
//
// The problem it solves: a transport has to decide what status an error
// deserves, but the packages that produce errors must not know about HTTP.
// Without a shared vocabulary the mapping site grows a case per package, and
// then a second copy of it per transport.
//
// So there are three layers, and what each one may *not* know is the point:
//
//   - A package returns its own errors, classified with a Kind. It never names
//     a status code, so the same error is usable from the HTTP handler, the
//     queue worker and a test with no transport at all.
//   - Kind is a closed set, small enough to write down, modelled on the
//     canonical gRPC code set that Connect already speaks.
//   - Each transport keeps one constant-size table from Kind to its own codes.
//     Adding a package does not change it.
//
// Classification survives wrapping: errors are wrapped with %w and KindOf
// walks the chain, so context added on the way out never loses it.
package apperr

// Kind classifies an error well enough for any transport to map it.
//
// Internal is deliberately the zero value: an error nobody classified is a bug
// on our side, and the safe answer to a bug is 500 and a log line, never a
// guess that happens to look like a client error.
type Kind uint8

const (
	// Internal is an unclassified failure. The client learns nothing beyond
	// the request id; the detail goes to the log.
	Internal Kind = iota

	// Invalid means the caller sent something unusable. Safe to describe back
	// to them: it is their own input.
	Invalid

	// Unauthenticated means the caller did not prove who they are.
	Unauthenticated

	// Forbidden means the caller is known but not allowed.
	Forbidden

	// NotFound means the addressed thing does not exist.
	NotFound

	// Conflict means someone else changed it first, or the current state does
	// not permit the operation.
	Conflict

	// Unprocessable means the request parsed and was well-formed, but is
	// semantically wrong — an idempotency key reused with a different body,
	// for one.
	Unprocessable

	// Exhausted means a quota or rate limit was reached.
	Exhausted

	// Unavailable means a dependency is down or a circuit is open. It says
	// "try again", which is what makes it different from Internal.
	Unavailable

	// Timeout means the work did not finish inside its deadline.
	Timeout

	// Canceled means the caller went away.
	Canceled
)

// normalize collapses any value outside the defined set onto Internal.
//
// A Kind can arrive out of range through a bad cast or a stale build, and it
// must behave the same way an unclassified error does: 500, retryable, logged.
// Doing that here means the three methods below cannot disagree about it.
func (k Kind) normalize() Kind {
	if k > Canceled {
		return Internal
	}
	return k
}

// String names the kind, for logs and for the metric label.
func (k Kind) String() string {
	switch k.normalize() {
	case Invalid:
		return "invalid"
	case Unauthenticated:
		return "unauthenticated"
	case Forbidden:
		return "forbidden"
	case NotFound:
		return "not_found"
	case Conflict:
		return "conflict"
	case Unprocessable:
		return "unprocessable"
	case Exhausted:
		return "exhausted"
	case Unavailable:
		return "unavailable"
	case Timeout:
		return "timeout"
	case Canceled:
		return "canceled"
	default:
		return "internal"
	}
}

// message is what a client is told when the error carries no message of its
// own. Every one of these is deliberately free of internal detail.
func (k Kind) message() string {
	switch k.normalize() {
	case Invalid:
		return "invalid argument"
	case Unauthenticated:
		return "unauthenticated"
	case Forbidden:
		return "forbidden"
	case NotFound:
		return "not found"
	case Conflict:
		return "conflict"
	case Unprocessable:
		return "unprocessable request"
	case Exhausted:
		return "too many requests"
	case Unavailable:
		return "temporarily unavailable"
	case Timeout:
		return "timed out"
	case Canceled:
		return "canceled"
	default:
		return "internal error"
	}
}

// Retryable reports whether trying the same request again could succeed
// without the caller changing anything.
//
// It is here rather than at a transport because the queue asks the same
// question of the same errors, and the answer must not differ between them:
// a job that fails with Unavailable is retried, one that fails with Invalid is
// a poison job and retrying it five times only hides the bug.
func (k Kind) Retryable() bool {
	switch k.normalize() {
	// Exhausted is the canonical "try again later": a 429 says the work was
	// refused for pacing, not rejected.
	//
	// Canceled is here because cancellation is never the work's own fault — a
	// handler cancelled by a shutdown or by losing its lease should be tried
	// again. Over HTTP it means the client went away and nobody retries
	// anyway, so including it costs nothing there.
	case Unavailable, Timeout, Internal, Canceled, Exhausted:
		return true
	default:
		return false
	}
}
