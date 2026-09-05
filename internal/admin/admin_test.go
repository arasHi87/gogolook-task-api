package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/admin"
)

func get(t *testing.T, h http.Handler, path string) (*http.Response, map[string]any) {
	t.Helper()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s returned %q: %v", path, rec.Body.String(), err)
	}
	return rec.Result(), body
}

// Liveness must not depend on anything outside the process. A liveness failure
// gets the container killed; if a shared database went down, every replica
// would be killed at once and would come back as a thundering herd against the
// thing that was already struggling.
func TestHealthzIgnoresDependencies(t *testing.T) {
	t.Parallel()

	h := admin.New(admin.Check{
		Name:  "database",
		Probe: func(context.Context) error { return errors.New("connection refused") },
	})

	resp, body := get(t, h.Mux(admin.Debug{}), "/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 with a failing dependency", resp.StatusCode)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %v, want ok", body["status"])
	}
}

func TestReadyzReportsEachCheck(t *testing.T) {
	t.Parallel()

	h := admin.New(
		admin.Check{Name: "database", Probe: func(context.Context) error { return nil }},
		admin.Check{Name: "queue", Probe: func(context.Context) error { return errors.New("boom") }},
	)

	resp, body := get(t, h.Mux(admin.Debug{}), "/readyz")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}

	checks, ok := body["checks"].(map[string]any)
	if !ok {
		t.Fatalf("no per-check detail in %v", body)
	}
	if checks["database"] != "ok" || checks["queue"] != "failed" {
		t.Errorf("checks = %v, want database ok and queue failed", checks)
	}
}

// The failing dependency's name is useful. Its error text may name a host, a
// port or a credential, so it stays in the process.
func TestReadyzDoesNotLeakProbeErrors(t *testing.T) {
	t.Parallel()

	h := admin.New(admin.Check{
		Name: "database",
		Probe: func(context.Context) error {
			return errors.New(`pq: password authentication failed for user "taskapi"`)
		},
	})

	rec := httptest.NewRecorder()
	h.Mux(admin.Debug{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if body := rec.Body.String(); strings.Contains(body, "password") || strings.Contains(body, "taskapi") {
		t.Errorf("the probe error leaked into the response: %s", body)
	}
}

func TestReadyzWithNoChecksIsReady(t *testing.T) {
	t.Parallel()

	resp, body := get(t, admin.New().Mux(admin.Debug{}), "/readyz")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200: nothing can be down", resp.StatusCode)
	}
	if body["status"] != "ready" {
		t.Errorf("status = %v, want ready", body["status"])
	}
}

// Draining is what takes a replica out of rotation before its listener stops
// accepting, so the requests that arrive in that window are still served.
func TestDrainingFlipsReadinessButNotLiveness(t *testing.T) {
	t.Parallel()

	h := admin.New()
	if h.Draining() {
		t.Fatal("draining before shutdown")
	}
	h.StartDraining()
	if !h.Draining() {
		t.Fatal("StartDraining did not take effect")
	}

	resp, body := get(t, h.Mux(admin.Debug{}), "/readyz")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("readyz = %d while draining, want 503", resp.StatusCode)
	}
	if body["status"] != "draining" {
		t.Errorf("status = %v, want draining", body["status"])
	}

	// Still alive: the process is finishing work, and killing it now would
	// drop exactly the requests the drain exists to protect.
	resp, _ = get(t, h.Mux(admin.Debug{}), "/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d while draining, want 200", resp.StatusCode)
	}
}

// A probe that hangs must not turn readiness into an outage of its own.
func TestReadyzBoundsSlowProbes(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	defer close(release)

	h := admin.New(admin.Check{
		Name: "slow",
		Probe: func(ctx context.Context) error {
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})

	started := time.Now()
	resp, _ := get(t, h.Mux(admin.Debug{}), "/readyz")
	elapsed := time.Since(started)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503: a probe that is slow is a probe that is failing", resp.StatusCode)
	}
	if elapsed > 5*time.Second {
		t.Errorf("readyz took %v; it must be bounded", elapsed)
	}
}

func TestVersionReportsBuildInfo(t *testing.T) {
	t.Parallel()

	resp, body := get(t, admin.New().Mux(admin.Debug{}), "/version")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	for _, k := range []string{"version", "commit", "go_version", "platform"} {
		if body[k] == "" || body[k] == nil {
			t.Errorf("version response is missing %s: %v", k, body)
		}
	}
}

// Nothing here may be cached: a stale readiness answer is worse than none.
func TestResponsesAreNotCacheable(t *testing.T) {
	t.Parallel()

	h := admin.New()
	for _, path := range []string{"/healthz", "/readyz", "/version"} {
		rec := httptest.NewRecorder()
		h.Mux(admin.Debug{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s Cache-Control = %q, want no-store", path, got)
		}
	}
}

func TestOnlyGETIsRouted(t *testing.T) {
	t.Parallel()

	h := admin.New()
	rec := httptest.NewRecorder()
	h.Mux(admin.Debug{}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if rec.Code == http.StatusOK {
		t.Error("POST /healthz was accepted")
	}
}
