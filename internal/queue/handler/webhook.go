// Package handler holds the job handlers.
//
// A handler is a plain function, and the package is separate from the queue so
// the queue has no opinion about what a job means.
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/failsafe-go/failsafe-go/budget"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"

	"github.com/arasHi87/gogolook-task-api/internal/apperr"
	"github.com/arasHi87/gogolook-task-api/internal/logging"
	"github.com/arasHi87/gogolook-task-api/internal/queue"
	"github.com/arasHi87/gogolook-task-api/internal/resilience"
	"github.com/arasHi87/gogolook-task-api/internal/task/pgrepo"
)

// Webhook delivers task events to a configured endpoint.
type Webhook struct {
	url     string
	client  *http.Client
	timeout time.Duration
	// guard is the timeout/retry/breaker chain. Nil disables it, which is what
	// a test that wants to observe one raw delivery asks for.
	guard *resilience.Executor
}

// Options configure a webhook handler.
type Options struct {
	URL     string
	Timeout time.Duration
	// Client is the HTTP client. Nil builds one with the timeout applied.
	Client *http.Client
	// Guard is the resilience chain. Nil means deliver directly.
	Guard *resilience.Executor
}

// NewWebhook returns a handler that POSTs events to url.
//
// An empty url makes delivery a no-op that succeeds, which is what a
// deployment with no sink configured wants: the events are still produced and
// still recorded, and turning the sink on later starts delivering without a
// migration.
func NewWebhook(o Options) *Webhook {
	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: o.Timeout}
	}
	return &Webhook{url: o.URL, client: client, timeout: o.Timeout, guard: o.Guard}
}

// IsDependencyFailure decides what the circuit breaker counts.
//
// This is the half of a breaker's configuration that is usually left wrong. A
// terminal error is our payload being rejected — our bug, not their outage —
// and counting it means one validation mistake takes down a healthy
// dependency, because a malformed request looks exactly like a broken server.
// Everything else that failed is theirs: a connection error, a timeout, a 5xx,
// a 429.
func IsDependencyFailure(err error) bool {
	return err != nil && !queue.IsTerminal(err)
}

// Handle delivers one event.
func (w *Webhook) Handle(ctx context.Context, j *queue.Job) error {
	event, err := queue.Unmarshal[pgrepo.Event](j)
	if err != nil {
		// A payload that will not parse will not parse on the fifth attempt
		// either. Unmarshal already marks it terminal.
		return err
	}

	if w.url == "" {
		logging.From(ctx).Debug("no webhook configured, event dropped",
			slog.String("event", event.Event))
		return nil
	}

	body, err := json.Marshal(event)
	if err != nil {
		return queue.Terminal(fmt.Errorf("marshal event: %w", err))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return queue.Terminal(fmt.Errorf("build request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")

	// The only honest answer for a side effect on something we cannot transact
	// with: tell the receiver enough to deduplicate, and document that it must.
	//
	// The idempotency key is per attempt-of-a-job and the event id is per
	// logical change, which are different questions: the first says "you have
	// seen this exact delivery", the second says "you have seen this change".
	req.Header.Set("Idempotency-Key", strconv.FormatInt(j.ID, 10)+":"+strconv.Itoa(j.Attempt))
	req.Header.Set("X-Event-Id", event.ID())

	return w.deliver(ctx, req)
}

// deliver sends one event through the resilience chain.
//
// An open circuit becomes a snooze, and that pairing is the whole reason both
// mechanisms exist here. The breaker turns a slow failure into a fast one;
// the snooze stops that fast failure being counted as the job's fault. Without
// it, a thirty-second dependency outage would burn a valid job's entire retry
// budget in a few milliseconds of instant refusals and discard it.
//
// The snooze waits for the circuit's own remaining delay, because rescheduling
// a job to run before the breaker will even probe is a guaranteed second
// failure.
func (w *Webhook) deliver(ctx context.Context, req *http.Request) error {
	if w.guard == nil {
		return w.send(req)
	}

	// The request body is a bytes.Reader, and a retry has to start from the
	// beginning of it. GetBody is what http.Client uses to rewind; setting it
	// here is what makes the retry policy above actually able to retry.
	err := w.guard.Run(func() error { return w.send(req.Clone(ctx)) })

	switch {
	case errors.Is(err, circuitbreaker.ErrOpen):
		return queue.Snooze(w.guard.RemainingDelay(),
			"circuit breaker open for "+w.guard.Name())
	case errors.Is(err, budget.ErrExceeded):
		// Retries are already saturating the budget, which means the fleet is
		// retrying hard. Adding to it would be the amplification the budget
		// exists to stop, so this job waits instead of failing.
		return queue.Snooze(w.timeout, "retry budget exhausted for "+w.guard.Name())
	default:
		return err
	}
}

// send performs one attempt.
func (w *Webhook) send(req *http.Request) error {
	resp, err := w.client.Do(req)
	if err != nil {
		// A transport failure is the dependency's problem, not the job's.
		return apperr.Wrap(apperr.Unavailable, err, "webhook delivery failed")
	}
	defer resp.Body.Close() //nolint:errcheck // response body is discarded

	return classify(resp.StatusCode)
}

// classify turns a response status into an outcome.
//
// The split matters more than it looks. A 4xx other than 429 is our bug — a
// malformed payload, a rejected schema — and retrying it four more times only
// delays finding out. A 5xx or a 429 is theirs, and is exactly what retries
// and a circuit breaker exist for. Counting our own bugs as their outage is
// how a validation error takes down a healthy dependency.
func classify(status int) error {
	switch {
	case status >= 200 && status < 300:
		return nil

	case status == http.StatusTooManyRequests:
		return apperr.New(apperr.Exhausted, "webhook rate limited")

	case status >= 500:
		return apperr.New(apperr.Unavailable, "webhook returned %d", status)

	case status >= 400:
		return queue.Terminal(apperr.New(apperr.Invalid, "webhook rejected the event with %d", status))

	default:
		return apperr.New(apperr.Unavailable, "webhook returned %d", status)
	}
}
