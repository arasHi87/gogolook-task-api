package logging

import (
	"context"
	"log/slog"
)

type ctxKey struct{}

// nop is what FromContext returns when nothing has been installed and no
// default has been set. It discards everything, so library code can log
// unconditionally without a nil check.
var nop = slog.New(discardHandler{})

// Into returns a context carrying log. Handlers install a request-scoped logger
// (request_id, trace_id, client_id) once, and everything downstream picks it up
// with FromContext instead of threading a *slog.Logger through every signature.
func Into(ctx context.Context, log *slog.Logger) context.Context {
	if log == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, log)
}

// From returns the logger installed on ctx, falling back to slog.Default and
// then to a discarding logger. It never returns nil.
func From(ctx context.Context) *slog.Logger {
	if ctx != nil {
		if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
			return l
		}
	}
	if d := slog.Default(); d != nil {
		return d
	}
	return nop
}

// With derives a context whose logger carries the extra attributes. Use it to
// add scope (job_id, attempt) without losing what the caller already attached.
func With(ctx context.Context, attrs ...slog.Attr) context.Context {
	if len(attrs) == 0 {
		return ctx
	}
	args := make([]any, 0, len(attrs))
	for _, a := range attrs {
		args = append(args, a)
	}
	return Into(ctx, From(ctx).With(args...))
}

// Trace logs at the wire tier. It exists because slog has no Logger.Trace and
// callers should not have to remember the numeric level.
//
// Hot paths should guard the call site with Enabled to avoid building
// attributes that will be thrown away:
//
//	if logging.Enabled(ctx, logging.LevelTrace) {
//	    logging.Trace(ctx, "sql", slog.String("query", q), slog.Duration("dur", d))
//	}
func Trace(ctx context.Context, msg string, attrs ...slog.Attr) {
	From(ctx).LogAttrs(ctx, LevelTrace, msg, attrs...)
}

// Enabled reports whether a record at level would be emitted for this context.
func Enabled(ctx context.Context, level slog.Level) bool {
	return From(ctx).Enabled(ctx, level)
}

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (discardHandler) WithAttrs([]slog.Attr) slog.Handler        { return discardHandler{} }
func (discardHandler) WithGroup(string) slog.Handler             { return discardHandler{} }
