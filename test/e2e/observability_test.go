package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestTheAdminSurfaceIsOnTheAdminPort is a security test wearing an
// observability test's clothes.
//
// A heap profile is a memory dump and the config dump names every dependency
// the process talks to. Which port they answer on is the boundary, so the
// assertion that matters is not that /metrics works — it is that it does not
// work anywhere else.
func TestTheAdminSurfaceIsOnTheAdminPort(t *testing.T) {
	t.Parallel()

	h := start(t, withoutWorker())

	private := []string{"/metrics", "/debug/config", "/debug/log-level", "/debug/pprof/"}
	for _, path := range private {
		t.Run("admin"+path, func(t *testing.T) {
			if code, _ := h.get(h.adminURL(path)); code != http.StatusOK {
				t.Errorf("%s on the admin port: status %d, want 200", path, code)
			}
		})
		t.Run("public"+path, func(t *testing.T) {
			// Anything but 200. The public mux routes unknown paths to the
			// transcoder, which answers 404 or 405; what must never happen is
			// the endpoint answering.
			if code, body := h.get(h.publicURL(path)); code == http.StatusOK {
				t.Errorf("%s is served on the PUBLIC port: %s", path, body)
			}
		})
	}
}

// The metrics are only worth having if the labels are the ones the dashboards
// and the alert rules actually query.
func TestTheMetricsMatchWhatTheAlertsQuery(t *testing.T) {
	t.Parallel()

	h := start(t)

	created := h.API.create("observed", 0)
	h.API.update(created.ID, "observed", 1)
	h.awaitSettled(2, 30*time.Second)

	code, body := h.get(h.adminURL("/metrics"))
	if code != http.StatusOK {
		t.Fatalf("/metrics: status %d", code)
	}

	// Every name below is read by deploy/prometheus/rules.yml or by a panel in
	// deploy/grafana/dashboards. A rename that broke one of them would
	// otherwise show up as an empty graph nobody notices until an incident.
	want := []string{
		"http_server_requests_total",
		"http_server_request_duration_seconds_bucket",
		"http_server_active_requests",
		"ratelimit_decisions_total",
		"auth_decisions_total",
		"db_pool_connections",
		"build_info",
		"go_goroutines",
	}
	for _, name := range want {
		if !strings.Contains(body, name) {
			t.Errorf("%s is missing from /metrics", name)
		}
	}

	// The route label is templated, never the raw URL. A single unbounded
	// label is not a metrics problem, it is an outage.
	if strings.Contains(body, created.ID) {
		t.Errorf("a task id appears in the metrics; the route label is not templated")
	}
	if !strings.Contains(body, `route="/api/v1/tasks/{id}"`) {
		t.Error("the templated route label is missing")
	}
}

// The worker publishes the queue's backlog; the api does not, because a
// process that does not consume would be reporting on a queue it has nothing
// to do with — and two replicas reporting the same global gauge is a sum that
// double counts.
func TestOnlyTheConsumerReportsTheBacklog(t *testing.T) {
	t.Parallel()

	h := start(t)

	// The backlog gauges are queried on scrape, so an empty queue emits no
	// series at all — which is correct, and means the scrape has to happen
	// while something is actually pending. Holding the delivery open is what
	// keeps a job in the running state long enough to be observed.
	h.Sink.hold()
	h.API.create("backlog", 0)
	h.Sink.awaitCount(1, 30*time.Second)

	_, worker := h.get("http://" + h.worker.addr("admin") + "/metrics")
	h.Sink.release()

	if !strings.Contains(worker, "job_queue_oldest_pending_age_seconds") {
		t.Error("the worker does not publish the queue's health signal")
	}
	if !strings.Contains(worker, `job_queue_depth{kind="task.event",state="running"} 1`) {
		t.Error("the worker does not report the job it is currently running")
	}

	h.awaitSettled(1, 30*time.Second)
	if _, after := h.get("http://" + h.worker.addr("admin") + "/metrics"); !strings.Contains(after, "jobs_processed_total") {
		t.Error("the worker does not publish job outcomes")
	}

	_, api := h.get(h.adminURL("/metrics"))
	if strings.Contains(api, "job_queue_depth") {
		t.Error("the api publishes the queue backlog; two replicas would double count it")
	}
}

// Turning the verbosity up without a restart is the difference between
// debugging a process and restarting away the state that would have explained
// the problem.
func TestTheLogLevelCanBeChangedAtRuntime(t *testing.T) {
	t.Parallel()

	h := start(t, withoutWorker())

	// Reported in the same spelling the log records use, and accepted in any
	// case, so a GET then a PUT of what it returned round-trips.
	if _, body := h.get(h.adminURL("/debug/log-level")); !strings.Contains(strings.ToLower(body), "debug") {
		t.Fatalf("initial level: %s", body)
	}

	code, body := h.put(h.adminURL("/debug/log-level"), "warn")
	if code != http.StatusOK || !strings.Contains(strings.ToLower(body), "warn") {
		t.Fatalf("set level: status %d: %s", code, body)
	}
	if _, body := h.get(h.adminURL("/debug/log-level")); !strings.Contains(strings.ToLower(body), "warn") {
		t.Errorf("the level did not stick: %s", body)
	}

	if code, _ := h.put(h.adminURL("/debug/log-level"), "not-a-level"); code != http.StatusBadRequest {
		t.Errorf("an unknown level returned %d, want 400", code)
	}
}

// "Which configuration is it actually running" is a different question from
// "what is in the config file", and it is the one that is usually wrong.
func TestTheEffectiveConfigIsServedWithSecretsMasked(t *testing.T) {
	t.Parallel()

	h := start(t, withoutWorker())

	code, body := h.get(h.adminURL("/debug/config"))
	if code != http.StatusOK {
		t.Fatalf("/debug/config: status %d", code)
	}
	if !strings.Contains(body, "backend: postgres") {
		t.Errorf("the dump does not reflect the environment it was started with:\n%s", body)
	}
	// The DSN carries the password. It must be masked here for the same reason
	// it is masked in --print-config: this dump ends up in bug reports.
	if strings.Contains(body, "taskapi@") || strings.Contains(body, ":taskapi@") {
		t.Error("the database password appears in the config dump")
	}
}
