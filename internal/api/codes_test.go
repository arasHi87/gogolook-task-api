package api

import (
	"testing"

	"connectrpc.com/connect"

	"github.com/arasHi87/gogolook-task-api/internal/apperr"
)

// Every kind except Internal must have an entry, or an error classified with
// it silently becomes a 500. Internal is absent on purpose: it is the
// unclassified path, which logs before returning.
func TestEveryKindIsMapped(t *testing.T) {
	t.Parallel()

	kinds := []apperr.Kind{
		apperr.Invalid, apperr.Unauthenticated, apperr.Forbidden, apperr.NotFound,
		apperr.Conflict, apperr.Unprocessable, apperr.Exhausted, apperr.Unavailable,
		apperr.Timeout, apperr.Canceled,
	}
	for _, k := range kinds {
		if _, ok := codeByKind[k]; !ok {
			t.Errorf("kind %v has no connect code; it would return 500", k)
		}
	}

	if _, mapped := codeByKind[apperr.Internal]; mapped {
		t.Error("Internal is in the table; it must take the log-and-return path instead")
	}
	if len(codeByKind) != len(kinds) {
		t.Errorf("the table has %d entries for %d kinds; a kind was added or removed",
			len(codeByKind), len(kinds))
	}
}

// The statuses the exercise's contract depends on, spelled out. Connect fixes
// code-to-status, so this asserts the choice of code was right.
func TestCodesProduceTheIntendedStatuses(t *testing.T) {
	t.Parallel()

	want := map[apperr.Kind]int{
		apperr.Invalid:         400,
		apperr.Unauthenticated: 401,
		apperr.Forbidden:       403,
		apperr.NotFound:        404,
		apperr.Conflict:        409,
		apperr.Exhausted:       429,
		apperr.Unavailable:     503,
		apperr.Timeout:         504,
	}
	for kind, status := range want {
		code := codeByKind[kind]
		if got := connectHTTPStatus(code); got != status {
			t.Errorf("%v maps to %v, which is HTTP %d, want %d", kind, code, got, status)
		}
	}
}

// connectHTTPStatus is the Connect protocol's fixed code-to-status mapping.
// It is written out here rather than imported because connect-go does not
// export it, and the point of the test is to pin the numbers the contract
// promises.
func connectHTTPStatus(c connect.Code) int {
	switch c {
	case connect.CodeCanceled:
		return 499
	case connect.CodeInvalidArgument, connect.CodeFailedPrecondition, connect.CodeOutOfRange:
		return 400
	case connect.CodeDeadlineExceeded:
		return 504
	case connect.CodeNotFound:
		return 404
	case connect.CodeAlreadyExists, connect.CodeAborted:
		return 409
	case connect.CodePermissionDenied:
		return 403
	case connect.CodeResourceExhausted:
		return 429
	case connect.CodeUnimplemented:
		return 501
	case connect.CodeUnavailable:
		return 503
	case connect.CodeUnauthenticated:
		return 401
	default:
		return 500
	}
}
