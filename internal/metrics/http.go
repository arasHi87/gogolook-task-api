package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/arasHi87/gogolook-task-api/internal/auth"
	"github.com/arasHi87/gogolook-task-api/internal/httpx"
)

// HTTP is the RED view of the public listener: rate, errors, duration.
type HTTP struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	active   *prometheus.GaugeVec
	reqSize  *prometheus.HistogramVec
	respSize *prometheus.HistogramVec
}

func newHTTP(reg prometheus.Registerer, native bool) *HTTP {
	h := &HTTP{
		// client_id is on the counter and nowhere else, and that split is the
		// cardinality decision. Per-tenant rate and error ratio is worth real
		// money during an incident; the same dimension on a fifteen-bucket
		// histogram multiplies the series by fifteen for a per-tenant p99
		// nobody has ever asked for. The label is bounded by the configured
		// client list plus the literal "anonymous" — never an address.
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "http_server_requests_total",
			Help:      "Requests handled, by templated route, status and client.",
		}, []string{"method", "route", "status", "client_id"}),

		duration: prometheus.NewHistogramVec(nativeOpts(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "http_server_request_duration_seconds",
			Help:      "Request duration, by templated route and status.",
			Buckets:   httpBuckets,
		}, native), []string{"method", "route", "status"}),

		active: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "http_server_active_requests",
			Help:      "Requests currently being served.",
		}, []string{"method", "route"}),

		reqSize: prometheus.NewHistogramVec(nativeOpts(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "http_server_request_body_size_bytes",
			Help:      "Request body size.",
			Buckets:   prometheus.ExponentialBuckets(64, 4, 8),
		}, native), []string{"method", "route"}),

		respSize: prometheus.NewHistogramVec(nativeOpts(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "http_server_response_body_size_bytes",
			Help:      "Response body size.",
			Buckets:   prometheus.ExponentialBuckets(64, 4, 8),
		}, native), []string{"method", "route"}),
	}

	reg.MustRegister(h.requests, h.duration, h.active, h.reqSize, h.respSize)
	return h
}

// Middleware records one request.
//
// It goes outermost in the chain, so a request refused by the in-flight
// limiter or the rate limiter is still counted. A 429 that does not appear in
// the request rate is a hole exactly where the dashboard is being read from.
func (h *HTTP) Middleware() httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Templated before the handler runs. The transcoder rewrites
			// r.URL.Path in place to the procedure it dispatched to, so
			// reading the path afterwards labels every REST request as the
			// same Connect route — the label would be bounded and wrong.
			route := httpx.Route(r.URL.Path)
			method := r.Method

			active := h.active.WithLabelValues(method, route)
			active.Inc()
			defer active.Dec()

			rec := &recorder{ResponseWriter: w, status: http.StatusOK}
			started := time.Now()
			next.ServeHTTP(rec, r)
			elapsed := time.Since(started)

			status := strconv.Itoa(rec.status)
			// Read after the handler: the identity is resolved by middleware
			// further in, so before it there is nothing to read.
			client := auth.From(r.Context()).ClientID

			h.requests.WithLabelValues(method, route, status, client).Inc()
			h.duration.WithLabelValues(method, route, status).Observe(elapsed.Seconds())
			h.respSize.WithLabelValues(method, route).Observe(float64(rec.written))
			if r.ContentLength > 0 {
				h.reqSize.WithLabelValues(method, route).Observe(float64(r.ContentLength))
			}
		})
	}
}

// recorder captures the status and the byte count.
//
// A near-twin of the one in httpx lives here rather than being shared, because
// sharing it would mean exporting a mutable wrapper across a package boundary
// for eight lines. The duplication is smaller than the coupling.
type recorder struct {
	http.ResponseWriter
	status  int
	written int64
	wrote   bool
}

func (r *recorder) WriteHeader(status int) {
	if r.wrote {
		return
	}
	r.wrote = true
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the real writer, so flushing and
// deadline control still work through the wrapper. connect-go type-asserts for
// http.Flusher, so this is load-bearing rather than tidy.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush forwards to the underlying writer when it can.
func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
