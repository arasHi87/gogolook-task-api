package idempotency_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/idempotency"
)

// counter is a handler that reports how many times it actually ran, which is
// the only thing any of these tests is really asking about.
type counter struct {
	runs   atomic.Int64
	status int
	block  chan struct{}
}

func (c *counter) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	n := c.runs.Add(1)
	if c.block != nil {
		<-c.block
	}

	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"run":%d}`, n)
}

func newStack(t *testing.T, h http.Handler, mutate ...func(*config.Idempotency)) (*httptest.Server, *idempotency.MemoryStore) {
	t.Helper()

	cfg := config.Defaults().Idempotency
	for _, m := range mutate {
		m(&cfg)
	}

	store := idempotency.NewMemoryStore()
	srv := httptest.NewServer(idempotency.Middleware(store, cfg)(h))
	t.Cleanup(srv.Close)
	return srv, store
}

// post sends a keyed write and returns the response with its body read.
func post(t *testing.T, srv *httptest.Server, key, body string) (*http.Response, string) {
	t.Helper()
	return send(t, srv, http.MethodPost, "/tasks", key, body)
}

func send(t *testing.T, srv *httptest.Server, method, path, key, body string) (*http.Response, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if key != "" {
		req.Header.Set(idempotency.HeaderKey, key)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // fully read below

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, string(raw)
}

// The whole point: the same request twice runs once.
func TestReplayReturnsTheStoredResponse(t *testing.T) {
	t.Parallel()

	h := &counter{}
	srv, _ := newStack(t, h)

	first, firstBody := post(t, srv, "key-1", `{"name":"a"}`)
	second, secondBody := post(t, srv, "key-1", `{"name":"a"}`)

	if n := h.runs.Load(); n != 1 {
		t.Errorf("the handler ran %d times, want 1", n)
	}
	if firstBody != secondBody {
		t.Errorf("replay body %q, want %q", secondBody, firstBody)
	}
	if second.StatusCode != first.StatusCode {
		t.Errorf("replay status %d, want %d", second.StatusCode, first.StatusCode)
	}

	// The header is how a client verifies its own retry logic. Without it a
	// replay is indistinguishable from a second execution.
	if first.Header.Get(idempotency.HeaderReplayed) != "" {
		t.Error("the first response is marked as a replay")
	}
	if second.Header.Get(idempotency.HeaderReplayed) != "true" {
		t.Errorf("%s = %q, want true", idempotency.HeaderReplayed,
			second.Header.Get(idempotency.HeaderReplayed))
	}
}

// The same key with a different request is the client's bug, and returning the
// first request's response to it would be ours.
func TestKeyReuseWithADifferentBodyIsRejected(t *testing.T) {
	t.Parallel()

	h := &counter{}
	srv, _ := newStack(t, h)

	post(t, srv, "key-1", `{"name":"a"}`)
	resp, body := post(t, srv, "key-1", `{"name":"b"}`)

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status %d, want 422: %s", resp.StatusCode, body)
	}
	if n := h.runs.Load(); n != 1 {
		t.Errorf("the handler ran %d times; the rejected request must not execute", n)
	}
}

// A key held by a request that is still running has no response to replay, and
// blocking until it does would hold a connection on both sides for as long as
// the first request takes.
func TestConcurrentUseOfAKeyIsAConflict(t *testing.T) {
	t.Parallel()

	h := &counter{block: make(chan struct{})}
	srv, _ := newStack(t, h)

	// The first request is left parked inside the handler. Its goroutine is
	// waited on before the test returns: t.Context is cancelled at that point,
	// and a request still in flight would then fail from under the harness.
	done := make(chan struct{})
	go func() {
		defer close(done)
		post(t, srv, "key-1", `{"name":"a"}`)
	}()

	// Wait for the handler to actually be inside, so the second request meets
	// a reservation rather than racing the first one's arrival.
	for h.runs.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	resp, body := post(t, srv, "key-1", `{"name":"a"}`)
	close(h.block)
	<-done

	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status %d, want 409: %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After; the client is told to wait but not how long")
	}
}

// A failed request must not consume its key. Remembering it would refuse the
// client a retry of something that never happened.
func TestAFailedRequestReleasesItsKey(t *testing.T) {
	t.Parallel()

	h := &counter{status: http.StatusInternalServerError}
	srv, _ := newStack(t, h)

	if resp, body := post(t, srv, "key-1", `{"name":"a"}`); resp.StatusCode != 500 {
		t.Fatalf("status %d, want 500: %s", resp.StatusCode, body)
	}

	h.status = http.StatusOK
	resp, body := post(t, srv, "key-1", `{"name":"a"}`)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("retry status %d, want 200: %s", resp.StatusCode, body)
	}
	if n := h.runs.Load(); n != 2 {
		t.Errorf("the handler ran %d times; the retry of a failed request must execute", n)
	}
	if resp.Header.Get(idempotency.HeaderReplayed) != "" {
		t.Error("the retry was answered as a replay of a response that was never stored")
	}
}

// Two different keys are two different requests, however identical they look.
func TestDifferentKeysExecuteSeparately(t *testing.T) {
	t.Parallel()

	h := &counter{}
	srv, _ := newStack(t, h)

	post(t, srv, "key-1", `{"name":"a"}`)
	post(t, srv, "key-2", `{"name":"a"}`)

	if n := h.runs.Load(); n != 2 {
		t.Errorf("the handler ran %d times, want 2", n)
	}
}

// The assignment's contract sends no such header, so a request without one has
// to work exactly as it always did.
func TestAWriteWithNoKeyIsUntouched(t *testing.T) {
	t.Parallel()

	h := &counter{}
	srv, _ := newStack(t, h)

	post(t, srv, "", `{"name":"a"}`)
	post(t, srv, "", `{"name":"a"}`)

	if n := h.runs.Load(); n != 2 {
		t.Errorf("the handler ran %d times, want 2: an unkeyed write must not be deduplicated", n)
	}
}

// A read has nothing to deduplicate, and buffering its response to store it
// would be pure cost.
func TestSafeMethodsPassThrough(t *testing.T) {
	t.Parallel()

	h := &counter{}
	srv, _ := newStack(t, h)

	send(t, srv, http.MethodGet, "/tasks", "key-1", "")
	send(t, srv, http.MethodGet, "/tasks", "key-1", "")

	if n := h.runs.Load(); n != 2 {
		t.Errorf("the handler ran %d times, want 2: a GET must not be replayed", n)
	}
}

// PUT and DELETE are idempotent in what they leave behind, but not in what
// they emit: two identical PUTs bump the version twice and produce two events.
// The key is what makes the retry of a write a no-op rather than a second one.
func TestUnsafeMethodsBeyondPostAreCovered(t *testing.T) {
	t.Parallel()

	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			h := &counter{}
			srv, _ := newStack(t, h)

			send(t, srv, method, "/tasks/1", "key-1", `{"name":"a"}`)
			send(t, srv, method, "/tasks/1", "key-1", `{"name":"a"}`)

			if n := h.runs.Load(); n != 1 {
				t.Errorf("the handler ran %d times, want 1", n)
			}
		})
	}
}

func TestKeyValidationIsEnforced(t *testing.T) {
	t.Parallel()

	h := &counter{}
	srv, _ := newStack(t, h)

	resp, body := post(t, srv, strings.Repeat("k", 512), `{"name":"a"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400: %s", resp.StatusCode, body)
	}
	if n := h.runs.Load(); n != 0 {
		t.Errorf("the handler ran %d times; a rejected key must not execute", n)
	}
}

func TestRequiredModeRejectsAWriteWithNoKey(t *testing.T) {
	t.Parallel()

	h := &counter{}
	srv, _ := newStack(t, h, func(c *config.Idempotency) { c.Required = true })

	resp, body := post(t, srv, "", `{"name":"a"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400: %s", resp.StatusCode, body)
	}

	// And the error is a problem document, like every other error the chain
	// produces itself.
	var problem map[string]any
	if err := json.Unmarshal([]byte(body), &problem); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if problem["status"] != float64(http.StatusBadRequest) {
		t.Errorf("problem status = %v, want 400", problem["status"])
	}
}

func TestDisabledMiddlewareIsAPassThrough(t *testing.T) {
	t.Parallel()

	h := &counter{}
	srv, _ := newStack(t, h, func(c *config.Idempotency) { c.Enabled = false })

	post(t, srv, "key-1", `{"name":"a"}`)
	post(t, srv, "key-1", `{"name":"a"}`)

	if n := h.runs.Load(); n != 2 {
		t.Errorf("the handler ran %d times, want 2", n)
	}
}

// The handler must still see the body the client sent: it is read out to be
// hashed, and putting it back is easy to forget.
func TestTheHandlerStillSeesTheBody(t *testing.T) {
	t.Parallel()

	var got string
	srv, _ := newStack(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got = string(raw)
		w.WriteHeader(http.StatusOK)
	}))

	post(t, srv, "key-1", `{"name":"a"}`)
	if want := `{"name":"a"}`; got != want {
		t.Errorf("the handler read %q, want %q", got, want)
	}
}

// What gets stored has to be servable to any later retry, and a compressed
// body is only servable to a client that accepts that encoding. The handler
// compresses according to the request it is given, and this middleware sits
// outside it — so the encoding is negotiated away before the handler runs, and
// the stored bytes are the canonical representation.
func TestTheHandlerIsAskedForAnIdentityResponse(t *testing.T) {
	t.Parallel()

	var accept string
	srv, _ := newStack(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept-Encoding")
		w.WriteHeader(http.StatusOK)
	}))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/tasks",
		strings.NewReader(`{"name":"a"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set(idempotency.HeaderKey, "key-1")
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()

	if accept != "" {
		t.Errorf("the handler saw Accept-Encoding %q; the captured response would be encoded", accept)
	}
}
