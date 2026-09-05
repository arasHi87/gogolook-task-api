package apperr_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/apperr"
)

// The property the whole package rests on: context added on the way out must
// not lose the classification. Every layer wraps with %w, so a Kind set three
// calls down still reaches the transport.
func TestKindSurvivesWrapping(t *testing.T) {
	t.Parallel()

	base := apperr.New(apperr.NotFound, "task not found")
	cases := map[string]error{
		"bare":              base,
		"wrapped once":      fmt.Errorf("repo: %w", base),
		"wrapped deep":      fmt.Errorf("a: %w", fmt.Errorf("b: %w", fmt.Errorf("c: %w", base))),
		"wrapped by apperr": apperr.Wrap(apperr.NotFound, base, "task not found"),
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := apperr.KindOf(err); got != apperr.NotFound {
				t.Errorf("KindOf = %v, want not_found", got)
			}
		})
	}
}

// Internal is the zero value on purpose: an error nobody classified is a bug
// on our side, and the safe answer to a bug is 500, never a guess that happens
// to look like a client error.
func TestUnclassifiedIsInternal(t *testing.T) {
	t.Parallel()

	cases := map[string]error{
		"plain error":   errors.New("disk on fire"),
		"wrapped plain": fmt.Errorf("layer: %w", errors.New("disk on fire")),
		"nil":           nil,
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := apperr.KindOf(err); got != apperr.Internal {
				t.Errorf("KindOf = %v, want internal", got)
			}
		})
	}

	var zero apperr.Kind
	if zero != apperr.Internal {
		t.Error("the zero Kind is not Internal; an unset classification would not fail safe")
	}
}

// The standard library's errors cannot carry a Kind, so they are classified
// once here rather than at every call site that might see one.
func TestStandardLibraryErrorsAreClassified(t *testing.T) {
	t.Parallel()

	cases := map[error]apperr.Kind{
		context.Canceled:                          apperr.Canceled,
		context.DeadlineExceeded:                  apperr.Timeout,
		fmt.Errorf("query: %w", context.Canceled): apperr.Canceled,
	}
	for err, want := range cases {
		if got := apperr.KindOf(err); got != want {
			t.Errorf("KindOf(%v) = %v, want %v", err, got, want)
		}
	}
}

// A package sentinel works as a classification test, which is what lets
// errors.Is keep reading the way it always has.
func TestIsMatchesOnKind(t *testing.T) {
	t.Parallel()

	sentinel := apperr.New(apperr.Invalid, "invalid argument")
	field := apperr.Field("name", "must not be empty")

	if !errors.Is(field, sentinel) {
		t.Error("a field error does not match the Invalid sentinel; they are the same kind")
	}
	if !errors.Is(fmt.Errorf("wrapped: %w", field), sentinel) {
		t.Error("wrapping broke the sentinel match")
	}
	if errors.Is(apperr.New(apperr.NotFound, "gone"), sentinel) {
		t.Error("a not-found error matched the Invalid sentinel")
	}
	if errors.Is(errors.New("plain"), sentinel) {
		t.Error("an unclassified error matched a sentinel")
	}
}

// The split that lets a 500 be bare without throwing away the reason: Msg is
// client-safe, Err is not and never reaches a response.
func TestMessageIsClientSafe(t *testing.T) {
	t.Parallel()

	secret := errors.New(`pq: password authentication failed for user "taskapi"`)

	t.Run("a classified error shows its own message", func(t *testing.T) {
		t.Parallel()
		err := apperr.Wrap(apperr.Unavailable, secret, "storage is unavailable")

		if got := apperr.Message(err); got != "storage is unavailable" {
			t.Errorf("Message = %q, want the declared safe text", got)
		}
		if strings.Contains(apperr.Message(err), "password") {
			t.Error("the cause leaked into the client message")
		}
		// The log still gets everything.
		if !strings.Contains(err.Error(), "password") {
			t.Errorf("Error() lost the cause: %v", err)
		}
	})

	t.Run("an unclassified error shows nothing", func(t *testing.T) {
		t.Parallel()
		if got := apperr.Message(secret); got != "internal error" {
			t.Errorf("Message = %q, want the generic internal message", got)
		}
	})

	t.Run("a field error names the field", func(t *testing.T) {
		t.Parallel()
		if got := apperr.Message(apperr.Field("name", "must not be empty")); got != "name: must not be empty" {
			t.Errorf("Message = %q, want \"name: must not be empty\"", got)
		}
	})

	t.Run("an empty message falls back to the kind", func(t *testing.T) {
		t.Parallel()
		if got := apperr.Message(&apperr.Error{Kind: apperr.NotFound}); got != "not found" {
			t.Errorf("Message = %q, want the kind's default", got)
		}
	})
}

func TestFieldOf(t *testing.T) {
	t.Parallel()

	err := apperr.Field("page_size", "must be at most %d", 100)
	if got := apperr.FieldOf(err); got != "page_size" {
		t.Errorf("FieldOf = %q, want page_size", got)
	}
	if got := apperr.FieldOf(fmt.Errorf("layer: %w", err)); got != "page_size" {
		t.Errorf("FieldOf through a wrap = %q, want page_size", got)
	}
	if got := apperr.FieldOf(errors.New("plain")); got != "" {
		t.Errorf("FieldOf(plain) = %q, want empty", got)
	}
}

// Sentinels are package-level values shared by every request. Nothing may
// mutate a classified error in place, or one request's annotation becomes
// every request's — and a data race besides.
func TestSentinelsAreNotMutatedByUse(t *testing.T) {
	t.Parallel()

	sentinel := apperr.New(apperr.NotFound, "task not found")
	before := sentinel.Error()

	wrapped := fmt.Errorf("repo: %w", sentinel)
	_ = apperr.KindOf(wrapped)
	_ = apperr.Message(wrapped)
	_ = apperr.FieldOf(wrapped)
	_ = errors.Is(wrapped, sentinel)

	if got := sentinel.Error(); got != before {
		t.Errorf("the sentinel changed from %q to %q just by being inspected", before, got)
	}
}

func TestIsHelper(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("wrapped: %w", apperr.New(apperr.Exhausted, "slow down"))
	if !apperr.Is(err, apperr.Exhausted) {
		t.Error("Is did not see through the wrap")
	}
	if apperr.Is(err, apperr.NotFound) {
		t.Error("Is matched the wrong kind")
	}
}
