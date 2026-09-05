package logging

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// traceHandler stamps the active trace on every record.
//
// A handler rather than something each call site adds, because the point is
// that it is on *every* record — including the ones written by code that knows
// nothing about tracing, which is most of it. An attribute that has to be
// remembered is an attribute that is missing from the one line that mattered.
//
// This is the join. A metric says the p99 moved and a log says a request was
// slow; neither says which request or where the time went. trace_id on the log
// line and an exemplar on the histogram both point at the same span, and that
// is what turns three signals into one investigation.
type traceHandler struct{ next slog.Handler }

// NewTraceHandler wraps h so that records written inside a sampled span carry
// trace_id and span_id.
func NewTraceHandler(h slog.Handler) slog.Handler { return &traceHandler{next: h} }

func (h *traceHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		// Only when sampled. An unsampled trace id points at a span nobody
		// stored, and a link that goes nowhere is worse than no link.
		if sc.IsSampled() {
			r.AddAttrs(
				slog.String("trace_id", sc.TraceID().String()),
				slog.String("span_id", sc.SpanID().String()),
			)
		}
	}
	return h.next.Handle(ctx, r)
}

func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{next: h.next.WithAttrs(attrs)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{next: h.next.WithGroup(name)}
}
