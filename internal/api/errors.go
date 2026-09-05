package api

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/arasHi87/gogolook-task-api/internal/logging"
	"github.com/arasHi87/gogolook-task-api/internal/task"
)

// toConnectError maps a domain error to a transport error. It is the only place
// that mapping happens, which is what keeps a storage error from ever reaching
// a client as itself.
//
//	domain error            connect code        HTTP
//	ErrNotFound             NotFound            404
//	ErrInvalidArgument      InvalidArgument     400
//	ErrConflict             Aborted             409
//	context.Canceled        Canceled            499
//	context.DeadlineExceeded DeadlineExceeded   504
//	anything else           Internal            500
//
// Anything unrecognised becomes a bare 500 carrying the request id and nothing
// else. The details go to the log, where they belong: an internal error message
// echoed to a client is an information leak and, more often, a confusing
// non-answer.
func toConnectError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}

	switch {
	case errors.Is(err, task.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("task not found"))

	case errors.Is(err, task.ErrInvalidArgument):
		// The field-scoped detail is safe to return: it is the caller's own
		// input being described back to them.
		var detail *task.InvalidArgumentError
		if errors.As(err, &detail) {
			return connect.NewError(connect.CodeInvalidArgument, errors.New(detail.Error()))
		}
		return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid argument"))

	case errors.Is(err, task.ErrConflict):
		return connect.NewError(connect.CodeAborted,
			errors.New("the task was modified by someone else; re-read it and retry"))

	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, errors.New("request canceled"))

	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("request timed out"))
	}

	// Unexpected. Log it with the request scope attached, return nothing.
	logging.From(ctx).Error("unhandled error", slog.Any("err", err))
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}
