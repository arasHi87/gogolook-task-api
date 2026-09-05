// Package metrics owns the Prometheus surface.
//
// Two rules shape everything here, and both are about the failure mode of
// metrics rather than their content.
//
// Cardinality is bounded by construction. Every label in this package has a
// value set I can name and count: the route is the templated path, never the
// raw URL; the status is the numeric code, never the message; the tier is one
// of three. No task ids, no error strings, and no IP addresses anywhere, ever.
// An unbounded label is not a metrics problem, it is an outage — one scanner
// walking /tasks/<random-uuid> mints a time series per request.
//
// Nothing here is exposed on the public listener. Metrics describe the inside
// of the process, and pprof next to them hands anyone who can reach the port a
// heap dump. The admin listener is a separate port so that stays a property of
// the deployment rather than a rule somebody has to remember.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/arasHi87/gogolook-task-api/internal/buildinfo"
	"github.com/arasHi87/gogolook-task-api/internal/config"
)

// namespace prefixes nothing.
//
// Deliberately: http_server_requests_total and the go_* and process_* series
// are conventional names an off-the-shelf dashboard already understands, and
// prefixing them with a service name makes every community query and every
// OTel-derived recording rule wrong for no gain. The service dimension belongs
// on the scrape job, where Prometheus puts it.
const namespace = ""

// httpBuckets are the OpenTelemetry HTTP semantic-convention defaults.
//
// Chosen rather than invented, because the useful property of a bucket set is
// that other people share it: an OTel-native backend, a community dashboard
// and a recording rule all assume these boundaries, and a bespoke set silently
// produces quantiles nobody can compare.
var httpBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10,
}

// jobBuckets are wider, because jobs are allowed to be slow. A webhook
// delivery that takes four seconds is a normal Tuesday; an API request that
// takes four seconds is an incident.
var jobBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// Registry is the metric set this process exposes.
type Registry struct {
	reg     *prometheus.Registry
	enabled bool

	HTTP   *HTTP
	Queue  *Queue
	Guards *Guards
	Deps   *Deps
}

// New builds the registry and everything registered in it.
//
// A private registry rather than prometheus.DefaultRegisterer: the default one
// is global mutable state that any imported package can write to, which makes
// "what does this process expose" unanswerable from the code. Ours is a value
// with an owner.
//
// Metrics are always collected, even when disabled. Collection is a handful of
// atomic adds and the alternative is a nil check at every call site, which is
// where instrumentation quietly stops happening. What the flag actually
// controls is exposure, which is the part that matters.
func New(cfg config.Metrics) *Registry {
	reg := prometheus.NewRegistry()

	r := &Registry{reg: reg, enabled: cfg.Enabled}
	native := cfg.NativeHistograms

	r.HTTP = newHTTP(reg, native)
	r.Queue = newQueue(reg, native)
	r.Guards = newGuards(reg)
	r.Deps = newDeps(reg, native)

	reg.MustRegister(
		// The modern runtime/metrics-backed go_* series: GC pause
		// distributions and scheduler latency, not just goroutine counts.
		collectors.NewGoCollector(
			collectors.WithGoCollectorRuntimeMetrics(collectors.MetricsScheduler, collectors.MetricsGC),
		),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo(),
	)
	return r
}

// buildInfo is the constant-1 gauge whose labels carry the version.
//
// A gauge rather than a log line, because it joins: `up * on(instance)
// build_info` answers "which version is the replica that is failing" without
// anyone having to correlate a deploy timestamp by hand.
func buildInfo() prometheus.Collector {
	info := buildinfo.Get()

	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "build_info",
		Help: "Build information. Always 1; the labels carry the value.",
	}, []string{"version", "commit", "go_version", "build_date"})

	g.WithLabelValues(info.Version, info.Commit, info.GoVersion, info.Date).Set(1)
	return g
}

// Register adds a collector, for the ones that need dependencies this package
// must not import — a database pool, a queue store.
func (r *Registry) Register(c prometheus.Collector) error { return r.reg.Register(c) }

// Enabled reports whether /metrics should be served.
func (r *Registry) Enabled() bool { return r.enabled }

// Handler serves the exposition format.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{
		// A scrape that fails should say so in the response rather than in a
		// log nobody is reading, and Prometheus records the failure either way.
		ErrorHandling: promhttp.HTTPErrorOnError,
		// Native histograms are only emitted when the scraper asks for
		// protobuf, so this is what makes the dual exposition actually dual.
		EnableOpenMetrics: true,
	})
}

// Gatherer exposes the registry for tests that assert on what was recorded.
func (r *Registry) Gatherer() prometheus.Gatherer { return r.reg }

// nativeOpts adds a native-histogram configuration to classic buckets.
//
// Both, on one metric. Native histograms are the modern answer — no bucket
// choice, roughly a tenth the series, quantiles at any resolution — but every
// existing dashboard and recording rule reads the classic buckets. Emitting
// both costs one extra field and makes the migration a scraper setting rather
// than a rewrite.
func nativeOpts(o prometheus.HistogramOpts, native bool) prometheus.HistogramOpts {
	if !native {
		return o
	}
	o.NativeHistogramBucketFactor = 1.1
	// Zero values are exact rather than bucketed below this width, and the
	// count is capped so a pathological distribution cannot grow the sparse
	// buckets without limit.
	o.NativeHistogramZeroThreshold = 1e-6
	o.NativeHistogramMaxBucketNumber = 160
	return o
}
