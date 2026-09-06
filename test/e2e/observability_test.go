package e2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/arasHi87/gogolook-task-api/test/harness"
)

// TestTheAdminSurfaceIsOnTheAdminPort is a security scenario wearing an
// observability scenario's clothes.
//
// A heap profile is a memory dump and the config dump names every dependency
// the process talks to. Which port they answer on is the boundary, so the
// assertion that matters is not that /metrics works — it is that it does not
// work anywhere else.
func TestTheAdminSurfaceIsOnTheAdminPort(t *testing.T) {
	t.Parallel()

	sys := harness.Start(t, harness.WithoutWorker())

	for _, path := range []string{"/metrics", "/debug/config", "/debug/log-level", "/debug/pprof/"} {
		sys.Admin.Get(path).Status(http.StatusOK)

		// Anything but 200. The public mux routes unknown paths to the
		// transcoder, which answers 404 or 405; what must never happen is the
		// endpoint answering.
		if resp := sys.Public.Get(path); resp.Code() == http.StatusOK {
			t.Errorf("%s is served on the PUBLIC port: %s", path, resp.Body)
		}
	}
}

// The metrics are only worth having if the labels are the ones the dashboards
// and the alert rules actually query.
func TestTheMetricsMatchWhatTheAlertsQuery(t *testing.T) {
	t.Parallel()

	sys := harness.Start(t)

	created := sys.API.Create("observed", 0)
	sys.API.Update(created.ID, "observed", 1)
	sys.Jobs.AwaitSettled(2)

	metrics := sys.Admin.Get("/metrics").Status(http.StatusOK)

	// Every name below is read by deploy/prometheus/rules.yml or by a panel in
	// deploy/grafana/dashboards. A rename that broke one of them would
	// otherwise show up as an empty graph nobody notices until an incident.
	for _, name := range []string{
		"http_server_requests_total",
		"http_server_request_duration_seconds_bucket",
		"http_server_active_requests",
		"ratelimit_decisions_total",
		"auth_decisions_total",
		"db_pool_connections",
		"build_info",
		"go_goroutines",
	} {
		metrics.BodyContains(name)
	}

	// The route label is templated, never the raw URL. A single unbounded
	// label is not a metrics problem, it is an outage.
	metrics.BodyOmits(created.ID).BodyContains(`route="/api/v1/tasks/{id}"`)
}

// The worker publishes the queue's backlog; the api does not, because a
// process that does not consume would be reporting on a queue it has nothing
// to do with — and two replicas reporting the same global gauge is a sum that
// double counts.
func TestOnlyTheConsumerReportsTheBacklog(t *testing.T) {
	t.Parallel()

	sys := harness.Start(t)

	// The backlog gauges are queried on scrape, so an empty queue emits no
	// series at all — which is correct, and means the scrape has to happen
	// while something is actually pending. Holding the delivery open is what
	// keeps a job in the running state long enough to be observed.
	sys.Webhook.Hold()
	sys.API.Create("backlog", 0)
	sys.Webhook.AwaitCount(1)

	worker := sys.WorkerMetrics()
	sys.Webhook.Release()

	for _, want := range []string{
		"job_queue_oldest_pending_age_seconds",
		`job_queue_depth{kind="task.event",state="running"} 1`,
	} {
		if !strings.Contains(worker, want) {
			t.Errorf("the worker does not publish %s", want)
		}
	}

	sys.Jobs.AwaitSettled(1)
	if !strings.Contains(sys.WorkerMetrics(), "jobs_processed_total") {
		t.Error("the worker does not publish job outcomes")
	}

	sys.Admin.Get("/metrics").BodyOmits("job_queue_depth")
}

// Turning the verbosity up without a restart is the difference between
// debugging a process and restarting away the state that would have explained
// the problem.
func TestTheLogLevelCanBeChangedAtRuntime(t *testing.T) {
	t.Parallel()

	sys := harness.Start(t, harness.WithoutWorker())

	// Reported in the same spelling the log records use, and accepted in any
	// case, so a GET then a PUT of what it returned round-trips.
	sys.Admin.Get("/debug/log-level").Status(http.StatusOK).BodyContains("DEBUG")

	sys.Admin.Put("/debug/log-level", "warn").Status(http.StatusOK).BodyContains("WARN")
	sys.Admin.Get("/debug/log-level").BodyContains("WARN")

	sys.Admin.Put("/debug/log-level", "not-a-level").Status(http.StatusBadRequest)
}

// "Which configuration is it actually running" is a different question from
// "what is in the config file", and it is the one that is usually wrong.
func TestTheEffectiveConfigIsServedWithSecretsMasked(t *testing.T) {
	t.Parallel()

	sys := harness.Start(t, harness.WithoutWorker())

	sys.Admin.Get("/debug/config").
		Status(http.StatusOK).
		BodyContains("backend: postgres").
		// The DSN carries the password. It must be masked here for the same
		// reason it is masked in --print-config: this dump ends up in bug
		// reports.
		BodyOmits(":taskapi@")
}
