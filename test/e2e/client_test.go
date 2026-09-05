package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/idempotency"
)

// client speaks the contract the assignment specifies, over a real socket to a
// real process.
//
// Every helper here is path-prefix agnostic: base carries the prefix, so the
// same client drives /api/v1/tasks and /tasks, and the contract test can prove
// the two surfaces are identical rather than assume it.
type client struct {
	t    *testing.T
	base string
	http *http.Client
}

func newClient(t *testing.T, base string) *client {
	t.Helper()
	return &client{
		t:    t,
		base: base,
		// Long enough for a cold start, short enough that a hung server fails
		// the test instead of hanging the suite until go test's own timeout.
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

// taskJSON is the wire shape, decoded loosely on purpose: the test asserts what
// the contract promises, not what the Go struct happens to contain.
type taskJSON struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    int    `json:"status"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type listJSON struct {
	Result        []taskJSON `json:"result"`
	NextPageToken string     `json:"next_page_token"`
}

// do issues a request and returns the response with its body already read, so
// callers never have to remember to close it.
func (c *client) do(method, path string, body any) (*http.Response, []byte) {
	c.t.Helper()

	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = bytes.NewBufferString(b)
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			c.t.Fatalf("encode body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(c.t.Context(), method, c.base+path, reader)
	if err != nil {
		c.t.Fatalf("%s %s: build request: %v", method, path, err)
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // body is fully read below

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return resp, raw
}

// keyed issues a request carrying an Idempotency-Key, and returns the response
// with its body as a string so a replay can be compared to the original byte
// for byte.
func (c *client) keyed(key, method, path, body string) (*http.Response, string) {
	c.t.Helper()

	req, err := http.NewRequestWithContext(c.t.Context(), method, c.base+path, bytes.NewBufferString(body))
	if err != nil {
		c.t.Fatalf("%s %s: build request: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(idempotency.HeaderKey, key)

	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // body is fully read below

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return resp, string(raw)
}

// bearer issues a request with an Authorization header, or without one when
// the token is empty.
func (c *client) bearer(token, method, path string) (*http.Response, string) {
	c.t.Helper()

	req, err := http.NewRequestWithContext(c.t.Context(), method, c.base+path, nil)
	if err != nil {
		c.t.Fatalf("%s %s: build request: %v", method, path, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // body is fully read below

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return resp, string(raw)
}

// unmarshal decodes a response body captured as a string.
func unmarshal(body string, out any) error { return json.Unmarshal([]byte(body), out) }

// expect issues a request, requires a status, and decodes the body into out.
func (c *client) expect(method, path string, body any, want int, out any) {
	c.t.Helper()

	resp, raw := c.do(method, path, body)
	if resp.StatusCode != want {
		c.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, want, raw)
	}
	if out == nil {
		return
	}
	if err := json.Unmarshal(raw, out); err != nil {
		c.t.Fatalf("%s %s: decode %s: %v", method, path, raw, err)
	}
}

func (c *client) create(name string, status int) taskJSON {
	c.t.Helper()

	var task taskJSON
	c.expect(http.MethodPost, "/tasks", map[string]any{"name": name, "status": status},
		http.StatusOK, &task)
	if task.ID == "" {
		c.t.Fatalf("create %q: response carries no id", name)
	}
	return task
}

func (c *client) update(id, name string, status int) taskJSON {
	c.t.Helper()

	var task taskJSON
	c.expect(http.MethodPut, "/tasks/"+id, map[string]any{"name": name, "status": status},
		http.StatusOK, &task)
	return task
}

func (c *client) remove(id string) {
	c.t.Helper()
	c.expect(http.MethodDelete, "/tasks/"+id, nil, http.StatusOK, nil)
}

func (c *client) list() listJSON {
	c.t.Helper()

	var page listJSON
	c.expect(http.MethodGet, "/tasks", nil, http.StatusOK, &page)
	return page
}
