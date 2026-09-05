package config_test

import (
	"strings"
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

func TestDefaultsAreValid(t *testing.T) {
	t.Parallel()
	c := config.Defaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("the built-in defaults must be valid, got: %v", err)
	}
}

// Validation reports everything wrong at once. A config file with four
// mistakes should be fixable in one pass, not five.
func TestValidateReportsEveryProblem(t *testing.T) {
	t.Parallel()
	c := config.Defaults()
	c.HTTP.Addr = ""
	c.Logging.Level = "loud"
	c.Queue.Workers = 0
	c.Auth.Mode = "maybe"

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted a config with four problems")
	}
	for _, want := range []string{"http.addr", "logging.level", "queue.workers", "auth.mode"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %s:\n%v", want, err)
		}
	}
}

func TestValidateCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(*config.Config)
		wantErr string
	}{
		{
			name:    "postgres backend needs a dsn",
			mutate:  func(c *config.Config) { c.Storage.Backend = config.BackendPostgres },
			wantErr: "storage.postgres.dsn",
		},
		{
			name: "postgres backend with a dsn is fine",
			mutate: func(c *config.Config) {
				c.Storage.Backend = config.BackendPostgres
				c.Storage.Postgres.DSN = "postgres://taskapi@localhost/tasks"
			},
		},
		{
			name:    "admin must not share the public port",
			mutate:  func(c *config.Config) { c.Admin.Addr = c.HTTP.Addr },
			wantErr: "admin.addr",
		},
		{
			// A heartbeat that is not comfortably inside the lease means one GC
			// pause costs the worker its jobs.
			name:    "heartbeat must fit three times inside the lease",
			mutate:  func(c *config.Config) { c.Queue.HeartbeatInterval = c.Queue.Lease },
			wantErr: "queue.heartbeat_interval",
		},
		{
			name:    "backoff base must not exceed max",
			mutate:  func(c *config.Config) { c.Queue.Backoff.Base = c.Queue.Backoff.Max + 1 },
			wantErr: "queue.backoff.base",
		},
		{
			name:    "unknown jitter strategy",
			mutate:  func(c *config.Config) { c.Queue.Backoff.Jitter = "sometimes" },
			wantErr: "queue.backoff.jitter",
		},
		{
			name: "token must be a sha256, never the token itself",
			mutate: func(c *config.Config) {
				c.Auth.Clients[0].TokenSHA256 = "demo-standard-token"
			},
			wantErr: "token_sha256",
		},
		{
			name: "duplicate client ids",
			mutate: func(c *config.Config) {
				c.Auth.Clients[1].ID = c.Auth.Clients[0].ID
			},
			wantErr: "duplicate client id",
		},
		{
			name: "duplicate tokens",
			mutate: func(c *config.Config) {
				c.Auth.Clients[1].TokenSHA256 = c.Auth.Clients[0].TokenSHA256
			},
			wantErr: "duplicate token",
		},
		{
			name: "auth required with no clients rejects everything",
			mutate: func(c *config.Config) {
				c.Auth.Mode = config.AuthRequired
				c.Auth.Clients = nil
			},
			wantErr: "auth.clients",
		},
		{
			// Without a throughput floor, one failure out of one request opens
			// the circuit. This is the most common breaker misconfiguration.
			name:    "breaker needs a throughput floor",
			mutate:  func(c *config.Config) { c.Breaker.Webhook.MinThroughput = 0 },
			wantErr: "min_throughput",
		},
		{
			name:    "breaker rate threshold above one",
			mutate:  func(c *config.Config) { c.Breaker.Webhook.FailureRateThreshold = 1.5 },
			wantErr: "failure_rate_threshold",
		},
		{
			name: "a circuit that can never close",
			mutate: func(c *config.Config) {
				c.Breaker.Webhook.HalfOpenMaxCalls = 2
				c.Breaker.Webhook.HalfOpenSuccessThreshold = 3
			},
			wantErr: "half_open_success_threshold",
		},
		{
			name:    "webhook url must be http(s)",
			mutate:  func(c *config.Config) { c.Webhook.URL = "ftp://sink/hook" },
			wantErr: "webhook.url",
		},
		{
			name:    "webhook url may be empty",
			mutate:  func(c *config.Config) { c.Webhook.URL = "" },
			wantErr: "",
		},
		{
			name:    "valid webhook url",
			mutate:  func(c *config.Config) { c.Webhook.URL = "http://webhook-sink:8080/hook" },
			wantErr: "",
		},
		{
			name:    "tracing enabled needs an endpoint",
			mutate:  func(c *config.Config) { c.Observability.Tracing.Enabled = true },
			wantErr: "otlp_endpoint",
		},
		{
			name:    "rate limit tiers need a positive rate",
			mutate:  func(c *config.Config) { c.RateLimit.Tiers.Anonymous.Rate = 0 },
			wantErr: "ratelimit.tiers.anonymous.rate",
		},
		{
			name: "disabled rate limiting skips its own validation",
			mutate: func(c *config.Config) {
				c.RateLimit.Enabled = false
				c.RateLimit.Tiers.Anonymous.Rate = 0
			},
			wantErr: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := config.Defaults()
			tc.mutate(&c)

			err := c.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("Validate() = nil, want an error mentioning %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("Validate() = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}
