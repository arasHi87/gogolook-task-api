package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/arasHi87/gogolook-task-api/internal/apperr"
	"github.com/arasHi87/gogolook-task-api/internal/queue"
	"github.com/arasHi87/gogolook-task-api/internal/queue/handler"
	"github.com/arasHi87/gogolook-task-api/internal/task/pgrepo"
)

func newJob(t *testing.T, event pgrepo.Event) *queue.Job {
	t.Helper()

	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &queue.Job{ID: 42, Kind: pgrepo.JobKind, Payload: payload, Attempt: 1, MaxAttempts: 5}
}

func testEvent() pgrepo.Event {
	return pgrepo.Event{
		Event: pgrepo.EventCreated, TaskID: uuid.New(), Version: 1,
		Name: "buy milk", Status: 0, OccurredAt: time.Now().UTC(),
	}
}

// The receiver cannot be transacted with, so the only honest answer is to tell
// it enough to deduplicate and document that it must.
func TestDeliverySendsDeduplicationHeaders(t *testing.T) {
	t.Parallel()

	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	event := testEvent()
	h := handler.NewWebhook(srv.URL, time.Second, srv.Client())
	if err := h.Handle(t.Context(), newJob(t, event)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Per delivery: "you have seen this exact attempt".
	if want := "42:1"; got.Get("Idempotency-Key") != want {
		t.Errorf("Idempotency-Key = %q, want %q", got.Get("Idempotency-Key"), want)
	}
	// Per change: "you have seen this version of this task". Different
	// questions, so both are sent.
	if want := event.TaskID.String() + ":1"; got.Get("X-Event-Id") != want {
		t.Errorf("X-Event-Id = %q, want %q", got.Get("X-Event-Id"), want)
	}
}

// The split that matters more than it looks: a 4xx other than 429 is our bug,
// and retrying it four more times only delays finding out. Counting our own
// bugs as their outage is how a validation error takes down a healthy
// dependency.
func TestStatusDecidesRetryability(t *testing.T) {
	t.Parallel()

	cases := map[int]struct {
		wantErr   bool
		terminal  bool
		retryable bool
	}{
		200: {wantErr: false},
		204: {wantErr: false},
		400: {wantErr: true, terminal: true},
		404: {wantErr: true, terminal: true},
		422: {wantErr: true, terminal: true},
		429: {wantErr: true, retryable: true},
		500: {wantErr: true, retryable: true},
		503: {wantErr: true, retryable: true},
	}

	for status, want := range cases {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()

			h := handler.NewWebhook(srv.URL, time.Second, srv.Client())
			err := h.Handle(t.Context(), newJob(t, testEvent()))

			if !want.wantErr {
				if err != nil {
					t.Fatalf("Handle = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Handle = nil, want an error for %d", status)
			}

			if got := isTerminal(err); got != want.terminal {
				t.Errorf("terminal = %v, want %v (%v)", got, want.terminal, err)
			}
			if want.retryable && !apperr.Retryable(err) {
				t.Errorf("%d should be retryable, got %v", status, err)
			}
		})
	}
}

// A transport failure is the dependency's problem, not the job's — and it is
// exactly what the circuit breaker will key on.
func TestTransportFailureIsUnavailable(t *testing.T) {
	t.Parallel()

	h := handler.NewWebhook("http://127.0.0.1:1/hook", 500*time.Millisecond, nil)
	err := h.Handle(t.Context(), newJob(t, testEvent()))
	if err == nil {
		t.Fatal("Handle = nil, want an error")
	}
	if got := apperr.KindOf(err); got != apperr.Unavailable {
		t.Errorf("kind = %v, want unavailable", got)
	}
	if !apperr.Retryable(err) {
		t.Error("a transport failure should be retryable")
	}
}

// A payload that will not parse will not parse on the fifth attempt either.
func TestUnparseablePayloadIsTerminal(t *testing.T) {
	t.Parallel()

	h := handler.NewWebhook("http://example.invalid/hook", time.Second, nil)
	err := h.Handle(t.Context(), &queue.Job{
		ID: 1, Kind: pgrepo.JobKind, Payload: []byte("not json"), Attempt: 1, MaxAttempts: 5,
	})
	if err == nil {
		t.Fatal("Handle = nil, want an error")
	}
	if !isTerminal(err) {
		t.Errorf("err = %v, want terminal", err)
	}
}

// No sink configured is a no-op that succeeds: the events are still produced
// and recorded, and turning a sink on later starts delivering with no
// migration.
func TestNoURLIsANoOp(t *testing.T) {
	t.Parallel()

	h := handler.NewWebhook("", time.Second, nil)
	if err := h.Handle(t.Context(), newJob(t, testEvent())); err != nil {
		t.Errorf("Handle = %v, want nil", err)
	}
}

// The handler must respect its deadline, or a hung sink becomes a stuck job.
func TestDeliveryRespectsTheContext(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	block := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	// Order matters: defers run last-registered-first, so the channel is
	// closed before Close waits for the handler. The other way round is a
	// deadlock — Close waits for a handler that is waiting for the close.
	defer srv.Close()
	defer close(block)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	h := handler.NewWebhook(srv.URL, time.Minute, srv.Client())
	started := time.Now()
	err := h.Handle(ctx, newJob(t, testEvent()))

	if err == nil {
		t.Fatal("Handle = nil, want a timeout")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("Handle took %v; the context deadline was ignored", elapsed)
	}
	if hits.Load() != 1 {
		t.Errorf("the sink saw %d requests, want 1", hits.Load())
	}
}

func isTerminal(err error) bool { return queue.IsTerminal(err) }
