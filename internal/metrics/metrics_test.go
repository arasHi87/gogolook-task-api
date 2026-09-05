package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/metrics"
)

func newRegistry(t *testing.T) *metrics.Registry {
	t.Helper()
	return metrics.New(config.Defaults().Observability.Metrics)
}

// gather returns one metric family by name.
func gather(t *testing.T, r *metrics.Registry, name string) *dto.MetricFamily {
	t.Helper()

	families, err := r.Gatherer().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

// labelsOf renders one metric's label set as a comparable string.
func labelsOf(m *dto.Metric) string {
	parts := make([]string, 0, len(m.GetLabel()))
	for _, l := range m.GetLabel() {
		parts = append(parts, l.GetName()+"="+l.GetValue())
	}
	return strings.Join(parts, ",")
}

func serve(t *testing.T, r *metrics.Registry, path string) {
	t.Helper()

	h := r.HTTP.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	req := httptest.NewRequest(http.MethodGet, path, nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
}

// The single most important property of this package. An unbounded label is
// not a metrics problem, it is an outage: one scanner walking /tasks/<uuid>
// mints a time series per request and takes the backend down with it.
func TestTheRouteLabelIsTemplated(t *testing.T) {
	t.Parallel()

	r := newRegistry(t)
	for _, id := range []string{
		"1f0c1b2e-0000-4000-8000-000000000000",
		"9a3f1c22-1111-4000-8000-000000000001",
		"9a3f1c22-2222-4000-8000-000000000002",
	} {
		serve(t, r, "/api/v1/tasks/"+id)
	}

	f := gather(t, r, "http_server_requests_total")
	if f == nil {
		t.Fatal("http_server_requests_total was not recorded")
	}
	if n := len(f.GetMetric()); n != 1 {
		t.Fatalf("%d series for three distinct ids, want 1: %v", n, seriesOf(f))
	}
	if got := labelsOf(f.GetMetric()[0]); !strings.Contains(got, "route=/api/v1/tasks/{id}") {
		t.Errorf("labels = %q, want the templated route", got)
	}
}

// An unrecognised path must collapse to one label rather than mint a series
// per URL, which is what makes the label set finite by construction.
func TestUnknownPathsCollapse(t *testing.T) {
	t.Parallel()

	r := newRegistry(t)
	for _, p := range []string{"/wp-login.php", "/.env", "/admin/../../etc/passwd"} {
		serve(t, r, p)
	}

	f := gather(t, r, "http_server_requests_total")
	if n := len(f.GetMetric()); n != 1 {
		t.Errorf("%d series for three scanner paths, want 1: %v", n, seriesOf(f))
	}
}

// The counter carries the tenant and the histogram does not. Per-tenant rate
// and error ratio is worth real money during an incident; the same dimension
// on a fifteen-bucket histogram multiplies the series by fifteen for a
// per-tenant p99 nobody asks for.
func TestTheDurationHistogramCarriesNoClient(t *testing.T) {
	t.Parallel()

	r := newRegistry(t)
	serve(t, r, "/api/v1/tasks")

	f := gather(t, r, "http_server_request_duration_seconds")
	if f == nil {
		t.Fatal("http_server_request_duration_seconds was not recorded")
	}
	for _, l := range f.GetMetric()[0].GetLabel() {
		if l.GetName() == "client_id" {
			t.Error("the duration histogram carries client_id")
		}
	}

	counter := gather(t, r, "http_server_requests_total")
	if !strings.Contains(labelsOf(counter.GetMetric()[0]), "client_id=") {
		t.Error("the request counter does not carry client_id")
	}
}

// build_info is a constant-1 gauge whose labels carry the value, so
// `up * on(instance) build_info` answers "which version is the failing
// replica" without correlating a deploy timestamp by hand.
func TestBuildInfoIsExposed(t *testing.T) {
	t.Parallel()

	f := gather(t, newRegistry(t), "build_info")
	if f == nil {
		t.Fatal("build_info is missing")
	}
	if got := f.GetMetric()[0].GetGauge().GetValue(); got != 1 {
		t.Errorf("build_info = %v, want 1", got)
	}
	for _, want := range []string{"version=", "commit=", "go_version="} {
		if !strings.Contains(labelsOf(f.GetMetric()[0]), want) {
			t.Errorf("build_info is missing %s", want)
		}
	}
}

// The runtime series are what turns "it got slow" into "it is in GC".
func TestRuntimeAndProcessCollectorsAreRegistered(t *testing.T) {
	t.Parallel()

	r := newRegistry(t)
	for _, name := range []string{"go_goroutines", "go_gc_duration_seconds", "process_open_fds"} {
		if gather(t, r, name) == nil {
			t.Errorf("%s is missing", name)
		}
	}
}

// Every guard reports, and every label it reports is one of a set I can name.
func TestGuardLabelsAreBounded(t *testing.T) {
	t.Parallel()

	r := newRegistry(t)
	g := r.Guards

	for range 3 {
		g.RateLimited(config.TierAnonymous, true)
	}
	g.RateLimited(config.TierStandard, false)
	g.Shed()
	g.Authenticated("anonymous")
	g.BreakerState("webhook", "closed", "open", metrics.BreakerOpen)

	f := gather(t, r, "ratelimit_decisions_total")
	if n := len(f.GetMetric()); n != 2 {
		t.Errorf("%d rate-limit series, want 2: %v", n, seriesOf(f))
	}

	state := gather(t, r, "circuit_breaker_state")
	if got := state.GetMetric()[0].GetGauge().GetValue(); got != metrics.BreakerOpen {
		t.Errorf("circuit_breaker_state = %v, want %v", got, metrics.BreakerOpen)
	}
	// Ordered by severity, so max-over-time on a timeline panel still means
	// "how bad did it get".
	if metrics.BreakerClosed >= metrics.BreakerHalfOpen || metrics.BreakerHalfOpen >= metrics.BreakerOpen {
		t.Error("the breaker state encoding is not ordered by severity")
	}
}

// The exposition has to actually parse, which is the one thing a unit test on
// the registry cannot assume.
func TestHandlerServesTheExpositionFormat(t *testing.T) {
	t.Parallel()

	r := newRegistry(t)
	serve(t, r, "/api/v1/tasks")

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE http_server_requests_total counter",
		"# TYPE build_info gauge",
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the exposition is missing %q", want)
		}
	}
}

func seriesOf(f *dto.MetricFamily) []string {
	out := make([]string, 0, len(f.GetMetric()))
	for _, m := range f.GetMetric() {
		out = append(out, labelsOf(m))
	}
	return out
}
