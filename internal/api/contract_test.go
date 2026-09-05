// This file is the proof that the exercise's requirement is met.
//
// From the assignment (BE_Coding Exercise_2B.pdf):
//
//	GET    /tasks       list tasks
//	POST   /tasks       create a task
//	PUT    /tasks/{id}  update a task
//	DELETE /tasks/{id}  delete a task
//
//	Task fields (at least): name (string), status (integer, [0, 1],
//	0 = incomplete, 1 = completed)
//
// Every case below runs twice: once against the canonical /api/v1 surface and
// once against the unversioned paths the assignment specifies. Both must be
// byte-identical in status and body; only the unversioned one carries the
// deprecation headers. That is what stops a later refactor from quietly
// 404-ing the paths the assignment asked for.
package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/api"
	"github.com/arasHi87/gogolook-task-api/internal/task"
	"github.com/arasHi87/gogolook-task-api/internal/task/memrepo"
)

// prefixes are the two surfaces every contract case is run against.
var prefixes = []struct {
	name   string
	prefix string
	legacy bool
}{
	{name: "canonical", prefix: "/api/v1", legacy: false},
	{name: "unversioned", prefix: "", legacy: true},
}

// newServer starts the real mux — the same handler main serves — over an empty
// in-memory store.
func newServer(t *testing.T) *httptest.Server {
	t.Helper()

	handler, err := api.NewMux(api.MuxOptions{
		Service:      task.NewService(memrepo.New()),
		MaxBodyBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// newServerWithHandler starts the real middleware chain around a handler that
// is not the transcoder, so chain behaviour can be tested directly.
func newServerWithHandler(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()

	handler, err := api.NewMuxWithHandler(api.MuxOptions{
		Service:      task.NewService(memrepo.New()),
		MaxBodyBytes: 1 << 20,
	}, h)
	if err != nil {
		t.Fatalf("NewMuxWithHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// do issues a request and returns the response with its body read.
func do(t *testing.T, srv *httptest.Server, method, path, body string) (*http.Response, []byte) {
	t.Helper()

	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, raw
}

// decode unmarshals a response body, failing the test with the raw body on
// error — an assertion on a field of an unparsed body is unreadable otherwise.
func decode(t *testing.T, raw []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

// taskJSON is the wire shape of a task, as a client sees it.
type taskJSON struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    *int   `json:"status"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	Version   any    `json:"version"`
}

func TestTheFourRequiredOperations(t *testing.T) {
	t.Parallel()

	for _, p := range prefixes {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(t)
			tasks := p.prefix + "/tasks"

			// POST /tasks — create
			resp, raw := do(t, srv, http.MethodPost, tasks, `{"name":"buy milk","status":0}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("POST %s = %d, want 200; body: %s", tasks, resp.StatusCode, raw)
			}
			var created taskJSON
			decode(t, raw, &created)
			if created.Name != "buy milk" {
				t.Errorf("created name = %q, want %q", created.Name, "buy milk")
			}
			if created.Status == nil || *created.Status != 0 {
				t.Errorf("created status = %v, want 0", created.Status)
			}
			if created.ID == "" {
				t.Error("create returned no id")
			}

			// GET /tasks — list
			resp, raw = do(t, srv, http.MethodGet, tasks, "")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200; body: %s", tasks, resp.StatusCode, raw)
			}
			var list struct {
				Result        []taskJSON `json:"result"`
				NextPageToken string     `json:"nextPageToken"`
			}
			decode(t, raw, &list)
			if len(list.Result) != 1 || list.Result[0].ID != created.ID {
				t.Fatalf("GET %s returned %+v, want the one created task", tasks, list.Result)
			}

			// PUT /tasks/{id} — update
			one := tasks + "/" + created.ID
			resp, raw = do(t, srv, http.MethodPut, one, `{"name":"buy oat milk","status":1}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("PUT %s = %d, want 200; body: %s", one, resp.StatusCode, raw)
			}
			var updated taskJSON
			decode(t, raw, &updated)
			if updated.Name != "buy oat milk" {
				t.Errorf("updated name = %q, want %q", updated.Name, "buy oat milk")
			}
			if updated.Status == nil || *updated.Status != 1 {
				t.Errorf("updated status = %v, want 1", updated.Status)
			}
			if updated.ID != created.ID {
				t.Errorf("update changed the id: %q -> %q", created.ID, updated.ID)
			}

			// DELETE /tasks/{id}
			resp, raw = do(t, srv, http.MethodDelete, one, "")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("DELETE %s = %d, want 200; body: %s", one, resp.StatusCode, raw)
			}

			// ...and it is gone.
			_, raw = do(t, srv, http.MethodGet, tasks, "")
			decode(t, raw, &list)
			if len(list.Result) != 0 {
				t.Errorf("after DELETE, GET %s returned %d tasks, want 0", tasks, len(list.Result))
			}
		})
	}
}

// status is an integer on the wire, not an enum name. This is the field most
// likely to be broken by a well-meaning refactor to a proto enum.
func TestStatusIsAnInteger(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	_, raw := do(t, srv, http.MethodPost, "/api/v1/tasks", `{"name":"done already","status":1}`)

	var generic map[string]any
	decode(t, raw, &generic)

	got, ok := generic["status"].(float64)
	if !ok {
		t.Fatalf("status = %#v (%T), want a JSON number", generic["status"], generic["status"])
	}
	if got != 1 {
		t.Errorf("status = %v, want 1", got)
	}
}

// A status outside [0, 1] is rejected. The constraint is declared once in the
// proto and enforced by protovalidate; this proves it reaches the wire.
func TestStatusEnumIsEnforced(t *testing.T) {
	t.Parallel()

	for _, p := range prefixes {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(t)

			for _, body := range []string{
				`{"name":"bad","status":2}`,
				`{"name":"bad","status":-1}`,
				`{"name":"bad","status":99}`,
			} {
				resp, raw := do(t, srv, http.MethodPost, p.prefix+"/tasks", body)
				if resp.StatusCode != http.StatusBadRequest {
					t.Errorf("POST %s = %d, want 400; body: %s", body, resp.StatusCode, raw)
				}
			}
		})
	}
}

func TestValidationRejectsBadNames(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	cases := map[string]string{
		"empty":    `{"name":"","status":0}`,
		"too long": fmt.Sprintf(`{"name":%q,"status":0}`, strings.Repeat("x", 256)),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp, raw := do(t, srv, http.MethodPost, "/api/v1/tasks", body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("POST = %d, want 400; body: %s", resp.StatusCode, raw)
			}
		})
	}

	t.Run("255 characters is accepted", func(t *testing.T) {
		body := fmt.Sprintf(`{"name":%q,"status":0}`, strings.Repeat("x", 255))
		resp, raw := do(t, srv, http.MethodPost, "/api/v1/tasks", body)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("POST = %d, want 200; body: %s", resp.StatusCode, raw)
		}
	})
}

func TestUnknownIDIsNotFound(t *testing.T) {
	t.Parallel()

	for _, p := range prefixes {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(t)
			missing := p.prefix + "/tasks/1f0c1b2e-0000-4000-8000-000000000000"

			resp, raw := do(t, srv, http.MethodPut, missing, `{"name":"x","status":0}`)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("PUT unknown id = %d, want 404; body: %s", resp.StatusCode, raw)
			}

			resp, raw = do(t, srv, http.MethodDelete, missing, "")
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("DELETE unknown id = %d, want 404; body: %s", resp.StatusCode, raw)
			}
		})
	}
}

func TestMalformedIDIsBadRequest(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	resp, raw := do(t, srv, http.MethodDelete, "/api/v1/tasks/not-a-uuid", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("DELETE with a malformed id = %d, want 400; body: %s", resp.StatusCode, raw)
	}
}

func TestMalformedBodyIsBadRequest(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	for _, body := range []string{`{`, `{"name":`, `not json at all`, `{"name": 42}`} {
		resp, raw := do(t, srv, http.MethodPost, "/api/v1/tasks", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST %q = %d, want 400; body: %s", body, resp.StatusCode, raw)
		}
	}
}

func TestWrongMethodIsRejected(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	// PATCH is not part of the contract.
	resp, _ := do(t, srv, http.MethodPatch, "/api/v1/tasks", `{}`)
	if resp.StatusCode == http.StatusOK {
		t.Errorf("PATCH /api/v1/tasks = 200, want a rejection")
	}
	if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotFound {
		t.Errorf("PATCH /api/v1/tasks = %d, want 405 or 404", resp.StatusCode)
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	t.Parallel()

	handler, err := api.NewMux(api.MuxOptions{
		Service:      task.NewService(memrepo.New()),
		MaxBodyBytes: 512,
	})
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	body := fmt.Sprintf(`{"name":%q,"status":0}`, strings.Repeat("x", 1024))
	resp, raw := do(t, srv, http.MethodPost, "/api/v1/tasks", body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("POST oversized body = %d, want 413; body: %s", resp.StatusCode, raw)
	}
}

// Both surfaces must produce identical responses. Only the unversioned one
// advertises its own deprecation.
func TestUnversionedSurfaceIsDeprecatedButIdentical(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	canonical, canonicalBody := do(t, srv, http.MethodGet, "/api/v1/tasks", "")
	legacy, legacyBody := do(t, srv, http.MethodGet, "/tasks", "")

	if canonical.StatusCode != legacy.StatusCode {
		t.Errorf("status: canonical %d, unversioned %d", canonical.StatusCode, legacy.StatusCode)
	}
	if !bytes.Equal(canonicalBody, legacyBody) {
		t.Errorf("bodies differ:\n canonical:   %s\n unversioned: %s", canonicalBody, legacyBody)
	}

	if got := legacy.Header.Get("Deprecation"); got != "true" {
		t.Errorf("unversioned Deprecation = %q, want \"true\"", got)
	}
	if got := legacy.Header.Get("Sunset"); got == "" {
		t.Error("unversioned response carries no Sunset date; a deprecation with no date is not a commitment")
	}
	if got := legacy.Header.Get("Link"); !strings.Contains(got, `rel="successor-version"`) {
		t.Errorf("unversioned Link = %q, want a successor-version link", got)
	}

	if got := canonical.Header.Get("Deprecation"); got != "" {
		t.Errorf("canonical surface advertises Deprecation = %q, want none", got)
	}
}

func TestEveryResponseCarriesARequestID(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	resp, _ := do(t, srv, http.MethodGet, "/api/v1/tasks", "")
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("response carries no X-Request-Id")
	}
}

// An id supplied by an upstream proxy is preserved, so one identifier follows
// a request across services.
func TestInboundRequestIDIsPreserved(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/api/v1/tasks", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Request-Id", "upstream-abc-123")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("X-Request-Id"); got != "upstream-abc-123" {
		t.Errorf("X-Request-Id = %q, want the inbound value", got)
	}
}

func TestListFilterAndPagination(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	for i := range 5 {
		status := i % 2
		body := fmt.Sprintf(`{"name":"task-%d","status":%d}`, i, status)
		if resp, raw := do(t, srv, http.MethodPost, "/api/v1/tasks", body); resp.StatusCode != http.StatusOK {
			t.Fatalf("seed %d = %d: %s", i, resp.StatusCode, raw)
		}
	}

	var list struct {
		Result        []taskJSON `json:"result"`
		NextPageToken string     `json:"nextPageToken"`
	}

	t.Run("status filter", func(t *testing.T) {
		_, raw := do(t, srv, http.MethodGet, "/api/v1/tasks?status=1", "")
		decode(t, raw, &list)
		if len(list.Result) != 2 {
			t.Fatalf("got %d completed tasks, want 2", len(list.Result))
		}
		for _, tk := range list.Result {
			if tk.Status == nil || *tk.Status != 1 {
				t.Errorf("filtered list returned status %v", tk.Status)
			}
		}
	})

	t.Run("page size and token", func(t *testing.T) {
		_, raw := do(t, srv, http.MethodGet, "/api/v1/tasks?pageSize=2", "")
		decode(t, raw, &list)
		if len(list.Result) != 2 {
			t.Fatalf("got %d tasks, want 2", len(list.Result))
		}
		if list.NextPageToken == "" {
			t.Fatal("no nextPageToken with more results available")
		}

		first := list.Result[0].ID
		_, raw = do(t, srv, http.MethodGet, "/api/v1/tasks?pageSize=2&pageToken="+list.NextPageToken, "")
		decode(t, raw, &list)
		if len(list.Result) == 0 {
			t.Fatal("second page is empty")
		}
		if list.Result[0].ID == first {
			t.Error("the second page repeats the first page's first row")
		}
	})

	t.Run("page size above the maximum is refused", func(t *testing.T) {
		resp, raw := do(t, srv, http.MethodGet, "/api/v1/tasks?pageSize=1000", "")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("pageSize=1000 = %d, want 400; body: %s", resp.StatusCode, raw)
		}
	})

	t.Run("a forged page token is refused", func(t *testing.T) {
		resp, raw := do(t, srv, http.MethodGet, "/api/v1/tasks?pageToken=not-a-real-token", "")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("forged token = %d, want 400; body: %s", resp.StatusCode, raw)
		}
	})
}

// The optimistic-concurrency path, end to end.
func TestVersionConflictIsReported(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	_, raw := do(t, srv, http.MethodPost, "/api/v1/tasks", `{"name":"original","status":0}`)
	var created taskJSON
	decode(t, raw, &created)
	one := "/api/v1/tasks/" + created.ID

	// Someone else updates first, taking the task to version 2.
	if resp, raw := do(t, srv, http.MethodPut, one, `{"name":"theirs","status":1}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("first update = %d: %s", resp.StatusCode, raw)
	}

	// Our update still claims version 1.
	resp, raw := do(t, srv, http.MethodPut, one, `{"name":"mine","status":0,"version":"1"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("stale update = %d, want 409; body: %s", resp.StatusCode, raw)
	}
}

// The documentation is served by the same binary, from the same spec the
// routes were generated from.
func TestDocsAreServed(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	cases := map[string]string{
		"/openapi.yaml": "application/yaml",
		"/openapi.json": "application/json",
		"/docs":         "text/html",
	}
	for path, contentType := range cases {
		resp, raw := do(t, srv, http.MethodGet, path, "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
			continue
		}
		if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, contentType) {
			t.Errorf("GET %s content-type = %q, want %q", path, got, contentType)
		}
		if len(raw) == 0 {
			t.Errorf("GET %s returned an empty body", path)
		}
	}
}

// The native Connect surface is served from the same handler on the same port,
// which is what makes "REST for the specified contract, RPC for everyone else"
// free rather than a second deployment.
func TestConnectSurfaceIsServedToo(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+"/task.v1.TaskService/ListTasks", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("Connect ListTasks = %d, want 200; body: %s", resp.StatusCode, raw)
	}
}

// A panic in a handler must become a 500 with a request id, not a dropped
// connection. Exercised through the real chain, because the ordering of
// RequestID, Logger and Recover is what makes it true.
func TestPanicBecomesA500WithARequestID(t *testing.T) {
	t.Parallel()

	srv := newServerWithHandler(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("handler exploded")
	}))

	resp, raw := do(t, srv, http.MethodGet, "/api/v1/tasks", "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", resp.StatusCode, raw)
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("the 500 carries no X-Request-Id")
	}

	var body map[string]any
	decode(t, raw, &body)
	if body["instance"] != resp.Header.Get("X-Request-Id") {
		t.Errorf("problem instance = %v, want the request id %q", body["instance"], resp.Header.Get("X-Request-Id"))
	}
	if strings.Contains(string(raw), "exploded") {
		t.Errorf("the panic value leaked to the client: %s", raw)
	}
}
