// Package tracing wires OpenTelemetry.
//
// It is off by default, and that is not laziness: an exporter with nowhere to
// send costs a goroutine, a connection attempt every few seconds and a queue
// that fills and drops, all to produce nothing. Tracing is switched on where
// there is a collector.
//
// What it buys, and the reason it is here at all, is the join. A metric says
// the p99 moved; a log says a request was slow; neither says *which* request,
// or where inside it the time went. A trace does, and an exemplar on the
// latency histogram makes a Grafana spike one click from the trace that caused
// it — which is the only version of this that anybody actually uses.
package tracing

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/arasHi87/gogolook-task-api/internal/buildinfo"
	"github.com/arasHi87/gogolook-task-api/internal/config"
)

// ScopeName identifies this service's instrumentation in the exported spans.
const ScopeName = "github.com/arasHi87/gogolook-task-api"

// Provider owns the exporter and the tracer.
type Provider struct {
	tracer   trace.Tracer
	shutdown func(context.Context) error
}

// New builds a provider. A disabled configuration returns one whose tracer is
// a no-op, so every call site is unconditional — a `if tracer != nil` at each
// span is where instrumentation quietly stops happening.
func New(cfg config.Tracing, service, instance string, log *slog.Logger) (*Provider, error) {
	if !cfg.Enabled {
		return &Provider{
			tracer:   noop.NewTracerProvider().Tracer(ScopeName),
			shutdown: func(context.Context) error { return nil },
		}, nil
	}

	// A bounded startup: an unreachable collector must not stop the service
	// from starting. The exporter connects lazily and retries on its own.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint),
		// Plaintext, because the collector is on the same network. A
		// deployment that crosses a boundary configures TLS here, and the
		// absence of that configuration is deliberate rather than forgotten.
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: build exporter: %w", err)
	}

	res := buildResource(service, instance, log)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		// ParentBased so a sampling decision made upstream is honoured: a
		// trace sampled at the edge and dropped here is a trace with a hole in
		// it, which is worse than not sampling it at all.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
	)

	otel.SetTracerProvider(tp)
	// W3C traceparent plus baggage. Without a propagator the spans are
	// produced and the trace still breaks at every process boundary, which is
	// the failure that looks most like "tracing works".
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	log.Info("tracing enabled",
		slog.String("endpoint", cfg.OTLPEndpoint),
		slog.Float64("sample_ratio", cfg.SampleRatio))

	return &Provider{tracer: tp.Tracer(ScopeName), shutdown: tp.Shutdown}, nil
}

// buildResource describes this process to the collector.
//
// The semconv version has to be the one the SDK's own default resource uses,
// because resource.Merge refuses to combine two different schema URLs. A
// mismatch is what a routine dependency bump produces, so the failure is
// downgraded to a warning and the attributes are used on their own: losing the
// host and runtime attributes degrades a trace, and refusing to start takes
// the service down for the sake of its telemetry.
func buildResource(service, instance string, log *slog.Logger) *resource.Resource {
	ours := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(service),
		semconv.ServiceVersion(buildinfo.Version()),
		semconv.ServiceInstanceID(instance),
	)

	merged, err := resource.Merge(resource.Default(), ours)
	if err != nil {
		log.Warn("tracing: could not merge the default resource; "+
			"spans will carry service attributes only",
			slog.Any("err", err))
		return ours
	}
	return merged
}

// Tracer returns the tracer to start spans with.
func (p *Provider) Tracer() trace.Tracer { return p.tracer }

// Shutdown flushes whatever is still batched.
//
// It runs last in the drain, and it matters: the batcher holds spans for up to
// five seconds, so a process that exits without this loses the spans from the
// incident that caused the restart.
func (p *Provider) Shutdown(ctx context.Context) error { return p.shutdown(ctx) }

// TraceID returns the current trace id, or "" outside a sampled trace.
//
// It is the join key between a log line, an exemplar and a span, which is why
// it is a plain string here rather than something typed: everything that
// consumes it wants the hex.
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

// SpanID returns the current span id, or "".
func SpanID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.SpanID().String()
}

// Attr is a convenience for the job attributes, so the key strings live in one
// place rather than being retyped at each span.
func Attr(key string, value any) attribute.KeyValue {
	switch v := value.(type) {
	case string:
		return attribute.String(key, v)
	case int:
		return attribute.Int(key, v)
	case int64:
		return attribute.Int64(key, v)
	case bool:
		return attribute.Bool(key, v)
	default:
		return attribute.String(key, fmt.Sprint(v))
	}
}
