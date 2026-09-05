package apperr_test

import (
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/apperr"
)

// The kinds are a closed set, small enough to write down. This is that list:
// if it changes, the transport tables that map from it have to change too, and
// this test is where a reader finds out what the vocabulary actually is.
var allKinds = []apperr.Kind{
	apperr.Internal,
	apperr.Invalid,
	apperr.Unauthenticated,
	apperr.Forbidden,
	apperr.NotFound,
	apperr.Conflict,
	apperr.Unprocessable,
	apperr.Exhausted,
	apperr.Unavailable,
	apperr.Timeout,
	apperr.Canceled,
}

// Every kind needs a distinct name, because the name becomes a metric label
// and a log field.
func TestKindNamesAreDistinct(t *testing.T) {
	t.Parallel()

	seen := map[string]apperr.Kind{}
	for _, k := range allKinds {
		name := k.String()
		if name == "" {
			t.Errorf("kind %d has no name", k)
		}
		if prev, dup := seen[name]; dup {
			t.Errorf("kinds %d and %d share the name %q", prev, k, name)
		}
		seen[name] = k
	}
}

// An unnamed kind must not silently render as something plausible. Anything
// outside the set is internal, which is the same way KindOf fails safe.
func TestUnknownKindIsInternal(t *testing.T) {
	t.Parallel()

	unknown := apperr.Kind(200)
	if got := unknown.String(); got != "internal" {
		t.Errorf("String() = %q, want internal", got)
	}
	if got := unknown.Retryable(); !got {
		t.Error("an unknown kind is not retryable; it should behave exactly like Internal")
	}
}

// Every kind needs a client-safe default message, or an error with no message
// of its own renders as nothing.
func TestEveryKindHasADefaultMessage(t *testing.T) {
	t.Parallel()

	for _, k := range allKinds {
		if got := apperr.Message(&apperr.Error{Kind: k}); got == "" {
			t.Errorf("kind %v has no default message", k)
		}
	}
}

// Retryable is here rather than at a transport because the queue asks the same
// question of the same errors, and the answer must not differ between them: a
// job that fails Invalid is a poison job, and retrying it five times only
// hides the bug.
func TestRetryable(t *testing.T) {
	t.Parallel()

	// The set is small and each member is here for a reason: the dependency
	// might recover, the work might be quicker next time, the bug might not
	// repeat, the pacing window will pass, or the cancellation was ours.
	retryable := map[apperr.Kind]bool{
		apperr.Unavailable: true,
		apperr.Timeout:     true,
		apperr.Internal:    true,
		apperr.Exhausted:   true,
		apperr.Canceled:    true,
	}
	for _, k := range allKinds {
		if got := k.Retryable(); got != retryable[k] {
			t.Errorf("%v.Retryable() = %v, want %v", k, got, retryable[k])
		}
	}

	// The caller's own mistakes are never worth retrying unchanged.
	for _, k := range []apperr.Kind{apperr.Invalid, apperr.NotFound, apperr.Conflict, apperr.Forbidden} {
		if k.Retryable() {
			t.Errorf("%v is retryable; retrying a caller error only hides the bug", k)
		}
	}
}
