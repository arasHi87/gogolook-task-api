// Package handler holds the job handlers.
//
// A handler is a plain function, and the package is separate from the queue so
// the queue has no opinion about what a job means.
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/apperr"
	"github.com/arasHi87/gogolook-task-api/internal/logging"
	"github.com/arasHi87/gogolook-task-api/internal/queue"
	"github.com/arasHi87/gogolook-task-api/internal/task/pgrepo"
)

// Webhook delivers task events to a configured endpoint.
type Webhook struct {
	url     string
	client  *http.Client
	timeout time.Duration
}

// NewWebhook returns a handler that POSTs events to url.
//
// An empty url makes delivery a no-op that succeeds, which is what a
// deployment with no sink configured wants: the events are still produced and
// still recorded, and turning the sink on later starts delivering without a
// migration.
func NewWebhook(url string, timeout time.Duration, client *http.Client) *Webhook {
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &Webhook{url: url, client: client, timeout: timeout}
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
	req.Header.Set("X-Event-Id", event.TaskID.String()+":"+strconv.FormatInt(event.Version, 10))

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
