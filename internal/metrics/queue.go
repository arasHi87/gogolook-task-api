package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/arasHi87/gogolook-task-api/internal/queue"
)

// Queue is the view that actually proves the system is working.
//
// The counters and histograms here are the throughput story. The gauges that
// matter most — depth and oldest-pending age — are not here at all: they come
// from a collector that queries on scrape, because a background goroutine
// writing gauges drifts and then lies after a scrape gap. See DepthCollector.
type Queue struct {
	processed  *prometheus.CounterVec
	enqueued   *prometheus.CounterVec
	retried    *prometheus.CounterVec
	leaseLost  *prometheus.CounterVec
	deadLetter *prometheus.CounterVec

	wait       *prometheus.HistogramVec
	processing *prometheus.HistogramVec
	claimBatch prometheus.Histogram

	workersConfigured prometheus.Gauge
}

func newQueue(reg prometheus.Registerer, native bool) *Queue {
	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: name, Help: help,
		}, labels)
	}

	q := &Queue{
		processed: counter("jobs_processed_total", "Jobs finalized, by kind and result.", "kind", "result"),
		enqueued:  counter("jobs_enqueued_total", "Jobs offered to the queue, by outcome.", "kind", "outcome"),
		retried:   counter("jobs_retried_total", "Jobs rescheduled after a failure.", "kind"),
		// No kind label: the reaper's UPDATE does not group by kind, and
		// adding a GROUP BY to a sweep that runs every fifteen seconds to
		// enrich a counter is the wrong trade. The result label carries what
		// matters — a reclaimed job that still had attempts left versus one
		// that did not.
		leaseLost:  counter("job_leases_expired_total", "Leases reclaimed by the reaper: a worker died or wedged.", "result"),
		deadLetter: counter("dead_letter_jobs_total", "Jobs that exhausted their attempts.", "kind"),

		// Enqueue to claim. This is the latency a producer actually
		// experiences, and it is the one depth cannot tell you about.
		wait: prometheus.NewHistogramVec(nativeOpts(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "job_wait_duration_seconds",
			Help:      "Time from enqueue to claim.",
			Buckets:   jobBuckets,
		}, native), []string{"kind"}),

		// Claim to finalize.
		processing: prometheus.NewHistogramVec(nativeOpts(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "job_processing_duration_seconds",
			Help:      "Time from claim to finalize, by result.",
			Buckets:   jobBuckets,
		}, native), []string{"kind", "result"}),

		// Claimed versus requested. A batch that comes back consistently short
		// of the configured size is workers contending on SKIP LOCKED, which
		// looks like nothing else in the metrics.
		claimBatch: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "job_claim_batch_size",
			Help:      "Jobs returned by one claim.",
			Buckets:   prometheus.LinearBuckets(0, 2, 11),
		}),

		workersConfigured: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "job_workers_configured",
			Help: "Workers this process runs.",
		}),
	}

	reg.MustRegister(
		q.processed, q.enqueued, q.retried, q.leaseLost, q.deadLetter,
		q.wait, q.processing, q.claimBatch, q.workersConfigured,
	)
	return q
}

// Configured records the pool size, so utilisation is a ratio rather than a
// number nobody can interpret.
//
// There is deliberately no per-replica "workers active" gauge to pair with it.
// The fleet-wide count of running jobs already comes from the scrape-time
// collector as job_queue_depth{state="running"}, and that number is true even
// for a replica that has wedged and stopped updating its own gauges — which is
// exactly the case anyone would be looking at it for.
func (q *Queue) Configured(n int) { q.workersConfigured.Set(float64(n)) }

// Finished records the outcome of one job.
func (q *Queue) Finished(e queue.Event) {
	result := string(e.Outcome)
	q.processed.WithLabelValues(e.Kind, result).Inc()
	q.processing.WithLabelValues(e.Kind, result).Observe(e.Took.Seconds())
	if e.Waited > 0 {
		q.wait.WithLabelValues(e.Kind).Observe(e.Waited.Seconds())
	}

	switch e.Outcome {
	case queue.OutcomeRetried:
		q.retried.WithLabelValues(e.Kind).Inc()
	case queue.OutcomeDiscarded:
		q.deadLetter.WithLabelValues(e.Kind).Inc()
	}
}

// Enqueued records a job offered to the queue. The deduped outcome is what
// makes the enqueue-time dedup visible rather than merely claimed.
func (q *Queue) Enqueued(kind, outcome string) { q.enqueued.WithLabelValues(kind, outcome).Inc() }

// Claimed records how many jobs one claim returned.
func (q *Queue) Claimed(n int) { q.claimBatch.Observe(float64(n)) }

// LeasesExpired records what the reaper reclaimed.
//
// Any value above zero means a worker died or wedged while holding work. It is
// self-healing, which is why it is a counter to alert on rather than an error,
// but it is never routine.
func (q *Queue) LeasesExpired(retryable, discarded int) {
	if retryable > 0 {
		q.leaseLost.WithLabelValues("retryable").Add(float64(retryable))
	}
	if discarded > 0 {
		q.leaseLost.WithLabelValues("discarded").Add(float64(discarded))
	}
}
