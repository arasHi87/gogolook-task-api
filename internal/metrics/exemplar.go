package metrics

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/arasHi87/gogolook-task-api/internal/tracing"
)

// observe records a duration, attaching the trace as an exemplar when there is
// one.
//
// This is the whole reason the three signals are worth having together. A
// latency histogram says the p99 moved and cannot say which request; a trace
// says where the time went in one request and cannot say whether that request
// was typical. An exemplar is a pointer from a bucket to a span that landed in
// it, so a spike on the dashboard is one click from the trace that caused it.
//
// Exemplars are only carried in the OpenMetrics exposition, which is why the
// handler negotiates it. A scraper that asks for plain text still gets the
// histogram, just without the pointers.
func observe(o prometheus.Observer, seconds float64, traceID string) {
	if traceID == "" {
		o.Observe(seconds)
		return
	}

	// Not every Observer can carry one — a plain histogram from an older
	// client, or a test double. Falling back rather than asserting means
	// instrumentation never panics for want of a pointer.
	ex, ok := o.(prometheus.ExemplarObserver)
	if !ok {
		o.Observe(seconds)
		return
	}

	// trace_id is the label Grafana and Prometheus both look for, and the
	// exemplar label set is capped at 128 UTF-8 runes by the spec — one label
	// with a 32-character hex value is the whole budget well spent.
	ex.ObserveWithExemplar(seconds, prometheus.Labels{"trace_id": traceID})
}

// observeCtx is observe for a caller that still holds the span.
func observeCtx(ctx context.Context, o prometheus.Observer, seconds float64) {
	observe(o, seconds, tracing.TraceID(ctx))
}
