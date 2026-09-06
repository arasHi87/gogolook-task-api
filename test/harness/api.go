package harness

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/idempotency"
)

// requestTimeout is long enough for a cold start and short enough that a hung
// server fails the scenario rather than hanging the suite until go test's own
// timeout, where the failure names nothing.
const requestTimeout = 15 * time.Second

// API drives the contract.
//
// It is immutable and its With* methods return a copy, so a scenario can hold
// one client for the versioned surface and another for the unversioned one
// without either affecting the other.
type API struct {
	t      *testing.T
	base   string
	client *http.Client
	key    string
	token  string
}

func newAPI(t *testing.T, base string) *API {
	t.Helper()
	return &API{t: t, base: base, client: &http.Client{Timeout: requestTimeout}}
}

// At returns a client for another path prefix on the same process, so the
// contract can be driven against /api/v1 and the unversioned surface the
// assignment specifies without starting a second system.
func (a *API) At(prefix string) *API {
	next := *a
	next.base = a.root() + prefix
	return &next
}

// WithKey returns a client that sends an Idempotency-Key.
func (a *API) WithKey(key string) *API {
	next := *a
	next.key = key
	return &next
}

// WithToken returns a client that authenticates, which is what selects the
// rate-limit tier.
func (a *API) WithToken(token string) *API {
	next := *a
	next.token = token
	return &next
}

// root is the scheme and host, without whatever prefix this client carries.
func (a *API) root() string {
	if i := strings.Index(a.base[len("http://"):], "/"); i >= 0 {
		return a.base[:len("http://")+i]
	}
	return a.base
}

// Task is the wire shape, decoded loosely on purpose: a scenario asserts what
// the contract promises, not what the Go struct happens to contain.
type Task struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    int    `json:"status"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Page is a list response.
type Page struct {
	Result        []Task `json:"result"`
	NextPageToken string `json:"next_page_token"`
}

// Create posts a task and requires it to succeed.
func (a *API) Create(name string, status int) Task {
	a.t.Helper()

	var task Task
	a.Post("/tasks", body(name, status)).Status(http.StatusOK).Into(&task)
	if task.ID == "" {
		a.t.Fatalf("create %q: the response carries no id", name)
	}
	return task
}

// Update replaces a task and requires it to succeed.
func (a *API) Update(id, name string, status int) Task {
	a.t.Helper()

	var task Task
	a.Put("/tasks/"+id, body(name, status)).Status(http.StatusOK).Into(&task)
	return task
}

// Delete removes a task and requires it to succeed.
func (a *API) Delete(id string) {
	a.t.Helper()
	a.Do(http.MethodDelete, "/tasks/"+id, "").Status(http.StatusOK)
}

// List reads the first page and requires it to succeed.
func (a *API) List() Page {
	a.t.Helper()

	var page Page
	a.Get("/tasks").Status(http.StatusOK).Into(&page)
	return page
}

// Get reads a path. It and its two siblings are the raw verbs, for the cases a
// scenario wants to assert on the response rather than take the happy path.
func (a *API) Get(path string) *Response { return a.Do(http.MethodGet, path, "") }

// Post writes to a path.
func (a *API) Post(path, b string) *Response { return a.Do(http.MethodPost, path, b) }

// Put replaces at a path.
func (a *API) Put(path, b string) *Response { return a.Do(http.MethodPut, path, b) }

// Do issues one request and reads the whole response, so a caller never has to
// remember to close it.
func (a *API) Do(method, path, payload string) *Response {
	a.t.Helper()

	var reader io.Reader
	if payload != "" {
		reader = bytes.NewBufferString(payload)
	}

	req, err := http.NewRequestWithContext(a.t.Context(), method, a.base+path, reader)
	if err != nil {
		a.t.Fatalf("%s %s: build request: %v", method, path, err)
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.key != "" {
		req.Header.Set(idempotency.HeaderKey, a.key)
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // fully read below

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		a.t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return &Response{t: a.t, what: method + " " + path, resp: resp, Body: string(raw)}
}

func body(name string, status int) string {
	encoded, _ := json.Marshal(map[string]any{"name": name, "status": status})
	return string(encoded)
}
