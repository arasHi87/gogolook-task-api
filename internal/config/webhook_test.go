package config_test

import (
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

// Delivery is opt-in: a bare `go run` must not try to call anything.
func TestWebhookDefaultsToNoTarget(t *testing.T) {
	t.Parallel()
	w := config.Defaults().Webhook

	if w.URL != "" {
		t.Errorf("url = %q, want empty", w.URL)
	}
	if w.Timeout <= 0 {
		t.Error("a breaker with no per-attempt timeout never trips; the calls just hang")
	}
	if w.Retry.MaxAttempts < 1 {
		t.Errorf("retry.max_attempts = %d, want at least 1", w.Retry.MaxAttempts)
	}
	wantNoProblem(t, check(&w))
}

func TestWebhookURLValidation(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		url   string
		field string
	}{
		"empty is allowed": {"", ""},
		"http":             {"http://webhook-sink:8080/hook", ""},
		"https":            {"https://example.test/hook", ""},
		"wrong scheme":     {"ftp://sink/hook", "webhook.url"},
		"no host":          {"http:///hook", "webhook.url"},
		"not a url at all": {"://nope", "webhook.url"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := config.Defaults().Webhook
			w.URL = tc.url

			err := check(&w)
			if tc.field == "" {
				wantNoProblem(t, err)
			} else {
				wantProblem(t, err, tc.field)
			}
		})
	}
}

func TestWebhookRetryValidation(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		mutate func(*config.Webhook)
		field  string
	}{
		"zero timeout":   {func(w *config.Webhook) { w.Timeout = 0 }, "webhook.timeout"},
		"no attempts":    {func(w *config.Webhook) { w.Retry.MaxAttempts = 0 }, "webhook.retry.max_attempts"},
		"zero base":      {func(w *config.Webhook) { w.Retry.Base = 0 }, "webhook.retry.base"},
		"base above max": {func(w *config.Webhook) { w.Retry.Base = w.Retry.Max + 1 }, "webhook.retry.base"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := config.Defaults().Webhook
			tc.mutate(&w)
			wantProblem(t, check(&w), tc.field)
		})
	}
}
