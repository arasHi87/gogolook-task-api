package harness

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// Response is one HTTP response and everything a scenario wants to say about
// it.
//
// Every assertion reports its own failure and returns the Response, so they
// chain and a scenario spends no lines on `if got != want`:
//
//	sys.API.Post("/tasks", `{"status":2}`).
//		Status(400).
//		BodyContains("must be in list")
type Response struct {
	t    *testing.T
	what string
	resp *http.Response
	// Body is the response body, read in full.
	Body string
}

// Code is the status, for the rare case a scenario needs to branch on it
// rather than assert it.
func (r *Response) Code() int { return r.resp.StatusCode }

// Header reads one response header.
func (r *Response) Header(name string) string { return r.resp.Header.Get(name) }

// Status requires an exact status code.
func (r *Response) Status(want int) *Response {
	r.t.Helper()
	if r.resp.StatusCode != want {
		r.t.Fatalf("%s: status %d, want %d: %s", r.what, r.resp.StatusCode, want, r.Body)
	}
	return r
}

// HasHeader requires a header to have an exact value.
func (r *Response) HasHeader(name, want string) *Response {
	r.t.Helper()
	if got := r.resp.Header.Get(name); got != want {
		r.t.Errorf("%s: %s = %q, want %q", r.what, name, got, want)
	}
	return r
}

// NoHeader requires a header to be absent. Asserting an absence is most of
// what a deprecation or a replay marker is worth.
func (r *Response) NoHeader(name string) *Response {
	r.t.Helper()
	if got := r.resp.Header.Get(name); got != "" {
		r.t.Errorf("%s: %s = %q, want it absent", r.what, name, got)
	}
	return r
}

// HeaderAtLeast requires a numeric header to be at least n. Retry-After of
// zero invites an immediate retry, which is the request that was just refused.
func (r *Response) HeaderAtLeast(name string, n int) *Response {
	r.t.Helper()

	raw := r.resp.Header.Get(name)
	got, err := strconv.Atoi(raw)
	if err != nil {
		r.t.Errorf("%s: %s = %q, want an integer", r.what, name, raw)
		return r
	}
	if got < n {
		r.t.Errorf("%s: %s = %d, want at least %d", r.what, name, got, n)
	}
	return r
}

// BodyContains requires a substring, for the errors whose exact shape is not
// the contract but whose explanation is.
func (r *Response) BodyContains(want string) *Response {
	r.t.Helper()
	if !strings.Contains(r.Body, want) {
		r.t.Errorf("%s: body does not mention %q: %s", r.what, want, r.Body)
	}
	return r
}

// BodyOmits requires a substring to be absent.
func (r *Response) BodyOmits(unwanted string) *Response {
	r.t.Helper()
	if strings.Contains(r.Body, unwanted) {
		r.t.Errorf("%s: body contains %q and should not: %s", r.what, unwanted, r.Body)
	}
	return r
}

// SameBodyAs requires two responses to be byte-identical.
//
// Byte-identical, not merely equivalent: a replay that re-renders its body is
// a different body to a client that compares, hashes or measures it, which is
// why the stored response is bytea rather than jsonb.
func (r *Response) SameBodyAs(other *Response) *Response {
	r.t.Helper()
	if r.Body != other.Body {
		r.t.Errorf("%s: body differs from %s\n got %s\nwant %s", r.what, other.what, r.Body, other.Body)
	}
	if r.resp.StatusCode != other.resp.StatusCode {
		r.t.Errorf("%s: status %d, want %s's %d",
			r.what, r.resp.StatusCode, other.what, other.resp.StatusCode)
	}
	return r
}

// Into decodes the body.
func (r *Response) Into(out any) *Response {
	r.t.Helper()
	if err := json.Unmarshal([]byte(r.Body), out); err != nil {
		r.t.Fatalf("%s: decode %s: %v", r.what, r.Body, err)
	}
	return r
}

// Task decodes the body as a task.
func (r *Response) Task() Task {
	r.t.Helper()

	var task Task
	r.Into(&task)
	return task
}
