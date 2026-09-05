package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Deps is what this process depends on.
//
// There is deliberately no per-query latency metric. It would need a pgx
// tracer on every statement, and what it would add over the pool collector's
// saturation view and the RED latency above is a breakdown by operation that
// nothing here is currently slow enough to need. A metric that is defined and
// never written is worse than an absent one: it draws an empty panel that
// reads as "the system is idle".
type Deps struct {
	requests *prometheus.HistogramVec
	retries  *prometheus.CounterVec

	configReloads *prometheus.CounterVec
}

func newDeps(reg prometheus.Registerer, native bool) *Deps {
	d := &Deps{
		requests: prometheus.NewHistogramVec(nativeOpts(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "dependency_request_duration_seconds",
			Help:      "Outbound request duration, by target and result.",
			Buckets:   httpBuckets,
		}, native), []string{"target", "result"}),

		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "dependency_retries_total",
			Help:      "Outbound attempts beyond the first.",
		}, []string{"target"}),

		configReloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "config_reloads_total",
			Help:      "SIGHUP reloads, by outcome.",
		}, []string{"result"}),
	}

	reg.MustRegister(d.requests, d.retries, d.configReloads)
	return d
}

// Request records one outbound call.
func (d *Deps) Request(target, result string, took time.Duration) {
	d.requests.WithLabelValues(target, result).Observe(took.Seconds())
}

// Retry records an attempt beyond the first.
func (d *Deps) Retry(target string) { d.retries.WithLabelValues(target).Inc() }

// ConfigReloaded records the outcome of a SIGHUP.
func (d *Deps) ConfigReloaded(result string) { d.configReloads.WithLabelValues(result).Inc() }
