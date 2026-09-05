package api

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/arasHi87/gogolook-task-api/internal/apperr"
	"github.com/arasHi87/gogolook-task-api/internal/logging"
)

// codeByKind maps the shared error vocabulary onto this transport's codes.
//
// It is the whole reason internal/apperr exists: the table is a fixed size no
// matter how many packages produce errors. A new package classifies its errors
// with an existing Kind and this file does not change.
//
// Connect's codes are the canonical gRPC set, and their HTTP statuses are
// fixed by the Connect protocol:
//
//	Kind             connect code         HTTP
//	Invalid          InvalidArgument      400
//	Unauthenticated  Unauthenticated      401
//	Forbidden        PermissionDenied     403
//	NotFound         NotFound             404
//	Conflict         Aborted              409
//	Unprocessable    InvalidArgument      400  (see below)
//	Exhausted        ResourceExhausted    429
//	Unavailable      Unavailable          503
//	Timeout          DeadlineExceeded     504
//	Canceled         Canceled             499
//	Internal         Internal             500
//
// Unprocessable has no code of its own, because the canonical set has nothing
// that maps to 422. Where 422 genuinely matters — a reused idempotency key
// with a different body — the middleware that detects it runs before the
// handler and writes the status directly.
var codeByKind = map[apperr.Kind]connect.Code{
	apperr.Invalid:         connect.CodeInvalidArgument,
	apperr.Unauthenticated: connect.CodeUnauthenticated,
	apperr.Forbidden:       connect.CodePermissionDenied,
	apperr.NotFound:        connect.CodeNotFound,
	apperr.Conflict:        connect.CodeAborted,
	apperr.Unprocessable:   connect.CodeInvalidArgument,
	apperr.Exhausted:       connect.CodeResourceExhausted,
	apperr.Unavailable:     connect.CodeUnavailable,
	apperr.Timeout:         connect.CodeDeadlineExceeded,
	apperr.Canceled:        connect.CodeCanceled,
}

// toConnectError maps a domain error to a transport error.
//
// It is the only place that mapping happens, which is what keeps a storage
// error from ever reaching a client as itself.
//
// Anything unclassified is Internal: a bare 500 carrying the request id and
// nothing else, with the detail written to the log. An internal error message
// echoed to a client is an information leak and, more often, a confusing
// non-answer.
func toConnectError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}

	code, classified := codeByKind[apperr.KindOf(err)]
	if !classified {
		logging.From(ctx).Error("unhandled error", slog.Any("err", err))
		code = connect.CodeInternal
	}

	// apperr.Message returns only what the error declared client-safe; the
	// wrapped cause stays in the log.
	return connect.NewError(code, errors.New(apperr.Message(err)))
}
