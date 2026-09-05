package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/arasHi87/gogolook-task-api/internal/apperr"
	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/queue"
	"github.com/arasHi87/gogolook-task-api/internal/queue/handler"
	"github.com/arasHi87/gogolook-task-api/internal/resilience"
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
	h := handler.NewWebhook(handler.Options{URL: srv.URL, Timeout: time.Second, Client: srv.Client()})
	if err := h.Handle(t.Context(), newJob(t, event)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Per delivery: "you have seen this exact attempt".
	if want := "42:1"; got.Get("Idempotency-Key") != want {
		t.Errorf("Idempotency-Key = %q, want %q", got.Get("Idempotency-Key"), want)
	}
	// Per change: "you have seen this change to this task". Different
	// questions, so both are sent.
	//
	// The event name leads. Without it a delete carries the same id as the
	// update before it — a delete removes the row at its current version and
	// invents no new one — and a receiver deduplicating on this header drops
	// the deletion.
	if want := event.ID(); got.Get("X-Event-Id") != want {
		t.Errorf("X-Event-Id = %q, want %q", got.Get("X-Event-Id"), want)
	}
	if want := event.Event + ":" + event.TaskID.String() + ":1"; event.ID() != want {
		t.Errorf("Event.ID() = %q, want %q", event.ID(), want)
	}
}

// TestDeleteIsNotAReplayOfTheUpdateBeforeIt pins the collision down.
//
// A delete carries the version of the row it removed, so the update that set
// that version and the delete that removed it describe different changes at
// the same version. If their ids matched, a receiver following our own
// instruction to deduplicate on X-Event-Id would silently drop every deletion.
func TestDeleteIsNotAReplayOfTheUpdateBeforeIt(t *testing.T) {
	t.Parallel()

	updated := testEvent()
	updated.Event = pgrepo.EventUpdated

	deleted := testEvent()
	deleted.Event = pgrepo.EventDeleted

	if updated.ID() == deleted.ID() {
		t.Fatalf("update and delete at version %d share the id %q", updated.Version, updated.ID())
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

			h := handler.NewWebhook(handler.Options{URL: srv.URL, Timeout: time.Second, Client: srv.Client()})
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

	h := handler.NewWebhook(handler.Options{URL: "http://127.0.0.1:1/hook", Timeout: 500 * time.Millisecond})
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

	h := handler.NewWebhook(handler.Options{URL: "http://example.invalid/hook", Timeout: time.Second})
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

	h := handler.NewWebhook(handler.Options{Timeout: time.Second})
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

	h := handler.NewWebhook(handler.Options{URL: srv.URL, Timeout: time.Minute, Client: srv.Client()})
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

// The breaker turns a slow failure into a fast one; the snooze stops that fast
// failure being counted as the job's fault.
//
// Without the pairing, a thirty-second dependency outage burns a valid job's
// entire retry budget in a few milliseconds of instant refusals and discards
// it — the job is punished for someone else's outage.
func TestAnOpenCircuitSnoozesRatherThanFails(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	h := handler.NewWebhook(handler.Options{
		URL:     srv.URL,
		Timeout: time.Second,
		Client:  srv.Client(),
		Guard:   testGuard(),
	})

	// Drive the circuit open. Four attempts is the configured floor.
	for range 4 {
		_ = h.Handle(t.Context(), newJob(t, testEvent()))
	}

	before := hits.Load()
	err := h.Handle(t.Context(), newJob(t, testEvent()))

	var snooze *queue.SnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("Handle = %v, want a snooze once the circuit is open", err)
	}
	if snooze.For <= 0 {
		t.Errorf("snooze duration = %s; the job would be retried before the circuit can probe", snooze.For)
	}
	if hits.Load() != before {
		t.Error("the sink was called through an open circuit")
	}

	// A snooze is not a failure: the job keeps its budget and comes back.
	if queue.IsTerminal(err) {
		t.Error("the snooze was terminal; the job would be discarded for a dependency outage")
	}
}

// The breaker must not count a rejected payload. Counting it means one
// validation bug takes down a healthy dependency.
func TestARejectedPayloadDoesNotOpenTheCircuit(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	h := handler.NewWebhook(handler.Options{
		URL:     srv.URL,
		Timeout: time.Second,
		Client:  srv.Client(),
		Guard:   testGuard(),
	})

	for range 20 {
		err := h.Handle(t.Context(), newJob(t, testEvent()))
		if !queue.IsTerminal(err) {
			t.Fatalf("Handle = %v, want a terminal error for a 400", err)
		}
		var snooze *queue.SnoozeError
		if errors.As(err, &snooze) {
			t.Fatal("the circuit opened on our own rejected payloads")
		}
	}
}

func TestIsDependencyFailure(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		err  error
		want bool
	}{
		"nil":                  {nil, false},
		"their outage":         {apperr.New(apperr.Unavailable, "503"), true},
		"their rate limit":     {apperr.New(apperr.Exhausted, "429"), true},
		"our rejected payload": {queue.Terminal(apperr.New(apperr.Invalid, "400")), false},
		"our unparseable job":  {queue.Terminal(errors.New("bad json")), false},
	}

	for name, tc := range cases {
		if got := handler.IsDependencyFailure(tc.err); got != tc.want {
			t.Errorf("%s: IsDependencyFailure = %v, want %v", name, got, tc.want)
		}
	}
}

// testGuard is the resilience chain with a low floor, so a test can trip it
// without making twenty calls.
func testGuard() *resilience.Executor {
	cfg := config.Defaults()

	b := cfg.Breaker.Webhook
	b.MinThroughput = 4
	b.Window = config.Duration(time.Minute)
	b.OpenDuration = config.Duration(30 * time.Second)

	r := cfg.Webhook.Retry
	r.MaxAttempts = 1
	r.Base = config.Duration(time.Millisecond)
	r.Max = config.Duration(2 * time.Millisecond)

	return resilience.New(resilience.Options{
		Name:      "test-webhook",
		Breaker:   b,
		Timeout:   time.Second,
		Retry:     r,
		IsFailure: handler.IsDependencyFailure,
	})
}

func isTerminal(err error) bool { return queue.IsTerminal(err) }
