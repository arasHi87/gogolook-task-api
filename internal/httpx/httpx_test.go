package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/httpx"
	"github.com/arasHi87/gogolook-task-api/internal/logging"
)

func TestRouteTemplating(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"/api/v1/tasks": "/api/v1/tasks",
		"/tasks":        "/tasks",
		"/api/v1/tasks/1f0c1b2e-0000-4000-8000-000000000000": "/api/v1/tasks/{id}",
		"/tasks/1f0c1b2e-0000-4000-8000-000000000000":        "/tasks/{id}",
		"/task.v1.TaskService/ListTasks":                     httpx.RouteConnect,
		"/healthz":                                           "/healthz",
		"/metrics":                                           "/metrics",
		"/openapi.yaml":                                      "/openapi.yaml",
		"/docs":                                              "/docs",
		"/debug/pprof/heap":                                  "/debug/*",
		"/tasks/abc/def":                                     httpx.RouteOther,
		"/../../etc/passwd":                                  httpx.RouteOther,
		"/":                                                  httpx.RouteOther,
		"/wp-admin/setup-config.php":                         httpx.RouteOther,
	}
	for path, want := range cases {
		if got := httpx.Route(path); got != want {
			t.Errorf("Route(%q) = %q, want %q", path, got, want)
		}
	}
}

// The route label is a metric dimension, so its value set must be finite no
// matter what a scanner sends.
func TestRouteLabelSetIsBounded(t *testing.T) {
	t.Parallel()
	seen := map[string]struct{}{}
	for i := range 500 {
		seen[httpx.Route("/tasks/"+strings.Repeat("a", i%40)+string(rune('0'+i%10)))] = struct{}{}
		seen[httpx.Route("/random/"+string(rune('a'+i%26)))] = struct{}{}
	}
	// /tasks/{id} and other — nothing else.
	if len(seen) != 2 {
		t.Errorf("unbounded input produced %d distinct route labels: %v", len(seen), seen)
	}
}

func TestIsLegacyRoute(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"/tasks":            true,
		"/tasks/abc":        true,
		"/api/v1/tasks":     false,
		"/api/v1/tasks/abc": false,
		"/tasks/abc/def":    false,
		"/tasksomething":    false,
	}
	for path, want := range cases {
		if got := httpx.IsLegacyRoute(path); got != want {
			t.Errorf("IsLegacyRoute(%q) = %v, want %v", path, got, want)
		}
	}
}

// The transcoder rewrites r.URL.Path in place. If the logger reads the path
// after the handler runs, every REST request is labelled as the same Connect
// route — a log that says nothing and a metric label that lies.
func TestLoggerTemplatesTheRouteBeforeTheHandlerRuns(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log, err := logging.New(logging.Options{Level: "debug", Format: "json", Writer: &buf, NoThrottle: true})
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}

	pathRewriter := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/task.v1.TaskService/DeleteTask" // what vanguard does
		w.WriteHeader(http.StatusNoContent)
	})

	handler := httpx.Chain(pathRewriter, httpx.RequestID(), httpx.Logger())

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/tasks/1f0c1b2e-0000-4000-8000-000000000000", nil)
	req = req.WithContext(logging.Into(context.Background(), log.Logger))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("decode %s: %v", buf.String(), err)
	}
	if rec["route"] != "/api/v1/tasks/{id}" {
		t.Errorf("route = %v, want /api/v1/tasks/{id}", rec["route"])
	}
	if rec["status"] != float64(http.StatusNoContent) {
		t.Errorf("status = %v, want 204", rec["status"])
	}
	if rec["request_id"] == "" || rec["request_id"] == nil {
		t.Error("the canonical log line carries no request_id")
	}
}

// Requests are a metric, not a log. At INFO the request log must be silent, or
// a thousand requests a second bury the six lines that matter.
func TestRequestLogIsSilentAtInfo(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log, err := logging.New(logging.Options{Level: "info", Format: "json", Writer: &buf, NoThrottle: true})
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}

	handler := httpx.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		httpx.RequestID(), httpx.Logger(),
	)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil)
	req = req.WithContext(logging.Into(context.Background(), log.Logger))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if buf.Len() != 0 {
		t.Errorf("request logging at INFO emitted %q, want nothing", buf.String())
	}
}

func TestRecoverTurnsAPanicIntoA500(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log, err := logging.New(logging.Options{Level: "error", Format: "json", Writer: &buf, NoThrottle: true})
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}

	handler := httpx.Chain(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }),
		httpx.RequestID(), httpx.Logger(), httpx.Recover(),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil)
	req = req.WithContext(logging.Into(context.Background(), log.Logger))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != httpx.ProblemContentType {
		t.Errorf("content-type = %q, want %q", ct, httpx.ProblemContentType)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	// The client gets the request id and nothing else; the panic value and its
	// stack go to the log.
	if body["instance"] == "" || body["instance"] == nil {
		t.Error("the error body carries no request id")
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("the panic value leaked to the client: %s", rec.Body.String())
	}
	if !strings.Contains(buf.String(), "boom") {
		t.Errorf("the panic was not logged: %s", buf.String())
	}
}

// A client that supplies a hostile request id must not have it reflected.
func TestRequestIDSanitisesInboundValues(t *testing.T) {
	t.Parallel()

	handler := httpx.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		httpx.RequestID(),
	)

	cases := map[string]bool{ // value -> should be preserved
		"upstream-abc-123":      true,
		strings.Repeat("x", 64): true,
		strings.Repeat("x", 65): false,
		"has\tcontrol":          false,
		"emoji-é":               false,
	}
	for value, preserved := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil)
		req.Header.Set(httpx.RequestIDHeader, value)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		got := rec.Header().Get(httpx.RequestIDHeader)
		if preserved && got != value {
			t.Errorf("id %q was replaced with %q, want it preserved", value, got)
		}
		if !preserved {
			if got == value {
				t.Errorf("hostile id %q was reflected back", value)
			}
			if got == "" {
				t.Errorf("id %q was rejected but no replacement was generated", value)
			}
		}
	}
}

// MaxBytesReader, not just the Content-Length check: a request that lies about
// its length must still be cut off.
func TestMaxBodyEnforcesTheLimitWhileReading(t *testing.T) {
	t.Parallel()

	var readErr error
	handler := httpx.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, readErr = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		}),
		httpx.RequestID(), httpx.MaxBody(16),
	)

	t.Run("honest Content-Length is rejected up front", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(strings.Repeat("x", 100)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413", rec.Code)
		}
	})

	t.Run("a body that lies about its length still fails", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(strings.Repeat("x", 100)))
		req.ContentLength = -1 // chunked: no declared length to check
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if readErr == nil {
			t.Error("reading an oversized chunked body succeeded; the limit is not enforced while reading")
		}
	})
}

func TestChainRunsOutermostFirst(t *testing.T) {
	t.Parallel()

	var order []string
	mw := func(name string) httpx.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}

	handler := httpx.Chain(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { order = append(order, "handler") }),
		mw("first"), mw("second"), mw("third"),
	)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if strings.Join(order, ",") != "first,second,third,handler" {
		t.Errorf("order = %v, want the written order", order)
	}
}

func TestDeprecationOnlyMarksTheUnversionedSurface(t *testing.T) {
	t.Parallel()

	handler := httpx.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		httpx.Deprecation(),
	)

	for path, deprecated := range map[string]bool{
		"/tasks":        true,
		"/tasks/abc":    true,
		"/api/v1/tasks": false,
		"/openapi.yaml": false,
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		got := rec.Header().Get("Deprecation") == "true"
		if got != deprecated {
			t.Errorf("%s: Deprecation present = %v, want %v", path, got, deprecated)
		}
		if deprecated {
			if rec.Header().Get("Sunset") == "" {
				t.Errorf("%s: no Sunset date", path)
			}
			if want := "</api/v1" + path + ">"; !strings.Contains(rec.Header().Get("Link"), want) {
				t.Errorf("%s: Link = %q, want it to point at %s", path, rec.Header().Get("Link"), want)
			}
		}
	}
}

var _ = slog.LevelDebug
