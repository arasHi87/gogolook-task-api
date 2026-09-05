package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/arasHi87/gogolook-task-api/internal/queue"
)

// scrapeTimeout bounds the queries these collectors run.
//
// A collector that hangs hangs the whole /metrics endpoint, and a scrape that
// never returns is worse than one that returns nothing: Prometheus waits, the
// scrape times out, and the gap looks like the process being down.
const scrapeTimeout = 2 * time.Second

// QueueCollector reports the backlog by querying on scrape.
//
// A collector rather than a background goroutine writing gauges, and the
// difference matters. A goroutine on a ticker reports whatever it last saw:
// after a pause, a stall or a scrape gap it keeps publishing a stale number
// with a fresh timestamp, which is not a delay, it is a lie. A collector
// cannot be stale, because it has no state to be stale with.
type QueueCollector struct {
	store *queue.Store
	log   *slog.Logger

	depth    *prometheus.Desc
	oldest   *prometheus.Desc
	failures prometheus.Counter
}

// NewQueueCollector builds the collector.
func NewQueueCollector(store *queue.Store, log *slog.Logger) *QueueCollector {
	if log == nil {
		log = slog.Default()
	}
	return &QueueCollector{
		store: store,
		log:   log.With(slog.String("component", "metrics")),
		depth: prometheus.NewDesc(
			"job_queue_depth",
			"Jobs waiting, by kind and state.",
			[]string{"kind", "state"}, nil),
		oldest: prometheus.NewDesc(
			"job_queue_oldest_pending_age_seconds",
			"Age of the oldest job that is due and not yet finished. The queue's primary health signal.",
			[]string{"kind"}, nil),
		failures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "job_queue_stats_errors_total",
			Help: "Scrapes that could not read the queue backlog.",
		}),
	}
}

// Describe implements prometheus.Collector.
func (c *QueueCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.depth
	ch <- c.oldest
	c.failures.Describe(ch)
}

// Collect implements prometheus.Collector.
func (c *QueueCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()

	stats, err := c.store.Stats(ctx)
	if err != nil {
		// Emitting nothing is right: a zero would be indistinguishable from an
		// empty queue, and "the backlog is fine" is the worst possible lie
		// during an incident. The counter is what makes the silence visible.
		c.failures.Inc()
		c.log.Warn("queue backlog scrape failed", slog.Any("err", err))
		c.failures.Collect(ch)
		return
	}

	// Oldest age is per kind, across every waiting state, so it has to be
	// folded here rather than emitted per row.
	oldest := map[string]float64{}
	for _, s := range stats {
		ch <- prometheus.MustNewConstMetric(c.depth, prometheus.GaugeValue,
			float64(s.Count), s.Kind, s.State)

		// A scheduled job whose time has not come is not late, so its negative
		// age is clamped rather than reported as the queue running ahead.
		age := max(s.OldestAge.Seconds(), 0)
		if age > oldest[s.Kind] {
			oldest[s.Kind] = age
		}
	}
	for kind, age := range oldest {
		ch <- prometheus.MustNewConstMetric(c.oldest, prometheus.GaugeValue, age, kind)
	}

	c.failures.Collect(ch)
}

// PoolCollector reports pgx pool utilisation.
//
// The USE view of the one resource that saturates first: a pool with every
// connection acquired is a service that is about to start queueing, and the
// symptom in the API metrics is latency with no obvious cause.
type PoolCollector struct {
	pool *pgxpool.Pool
	name string

	connections *prometheus.Desc
	acquires    *prometheus.Desc
	acquireWait *prometheus.Desc
}

// NewPoolCollector builds the collector. name distinguishes the api pool from
// the worker one, which is the whole point of them being separate: a worker
// saturated by slow jobs must not be able to starve the API, and this is where
// that shows up.
func NewPoolCollector(pool *pgxpool.Pool, name string) *PoolCollector {
	return &PoolCollector{
		pool: pool,
		name: name,
		connections: prometheus.NewDesc(
			"db_pool_connections",
			"Pool connections by state.",
			[]string{"pool", "state"}, nil),
		acquires: prometheus.NewDesc(
			"db_pool_acquires_total",
			"Connection acquisitions, by result.",
			[]string{"pool", "result"}, nil),
		acquireWait: prometheus.NewDesc(
			"db_pool_acquire_duration_seconds_total",
			"Cumulative time spent waiting for a connection.",
			[]string{"pool"}, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *PoolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.connections
	ch <- c.acquires
	ch <- c.acquireWait
}

// Collect implements prometheus.Collector.
//
// Cumulative counters rather than a histogram of acquire waits, and that is a
// limit worth stating: pgxpool reports totals, so a distribution would need a
// tracer on every acquisition. The total divided by the count is the mean,
// which is enough to see a pool going bad and not enough to see a tail.
func (c *PoolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()

	gauge := func(state string, v float64) {
		ch <- prometheus.MustNewConstMetric(c.connections, prometheus.GaugeValue, v, c.name, state)
	}
	gauge("acquired", float64(s.AcquiredConns()))
	gauge("idle", float64(s.IdleConns()))
	gauge("constructing", float64(s.ConstructingConns()))
	gauge("total", float64(s.TotalConns()))
	gauge("max", float64(s.MaxConns()))

	counter := func(result string, v float64) {
		ch <- prometheus.MustNewConstMetric(c.acquires, prometheus.CounterValue, v, c.name, result)
	}
	counter("ok", float64(s.AcquireCount()))
	counter("empty", float64(s.EmptyAcquireCount()))
	counter("canceled", float64(s.CanceledAcquireCount()))

	ch <- prometheus.MustNewConstMetric(c.acquireWait, prometheus.CounterValue,
		s.AcquireDuration().Seconds(), c.name)
}
