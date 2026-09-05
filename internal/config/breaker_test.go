package config_test

import (
	"testing"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

// The defaults are the values Resilience4j, Envoy and Polly converged on.
func TestBreakerDefaults(t *testing.T) {
	t.Parallel()
	b := config.Defaults().Breaker

	if b.Webhook.FailureRateThreshold != 0.5 {
		t.Errorf("failure_rate_threshold = %v, want 0.5: below about half you trip on normal noise",
			b.Webhook.FailureRateThreshold)
	}
	// Without a floor, one failure out of one request opens the circuit — the
	// single most common breaker misconfiguration.
	if b.Webhook.MinThroughput < 2 {
		t.Errorf("min_throughput = %d, want a real floor", b.Webhook.MinThroughput)
	}
	// One lucky probe is not recovery.
	if b.Webhook.HalfOpenSuccessThreshold < 2 {
		t.Errorf("half_open_success_threshold = %d, want more than one", b.Webhook.HalfOpenSuccessThreshold)
	}
	if b.Webhook.HalfOpenSuccessThreshold > b.Webhook.HalfOpenMaxCalls {
		t.Error("the circuit can never close: more successes required than probes allowed")
	}
	wantNoProblem(t, check(&b))
}

func TestBreakerValidate(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		mutate func(*config.Breaker)
		field  string
	}{
		"no throughput floor": {
			func(b *config.Breaker) { b.Webhook.MinThroughput = 0 },
			"breaker.webhook.min_throughput",
		},
		"threshold above one": {
			func(b *config.Breaker) { b.Webhook.FailureRateThreshold = 1.5 },
			"breaker.webhook.failure_rate_threshold",
		},
		"threshold zero": {
			func(b *config.Breaker) { b.Webhook.FailureRateThreshold = 0 },
			"breaker.webhook.failure_rate_threshold",
		},
		"zero window": {
			func(b *config.Breaker) { b.Webhook.Window = 0 },
			"breaker.webhook.window",
		},
		"zero open duration": {
			func(b *config.Breaker) { b.Webhook.OpenDuration = 0 },
			"breaker.webhook.open_duration",
		},
		"no probes allowed": {
			func(b *config.Breaker) { b.Webhook.HalfOpenMaxCalls = 0 },
			"breaker.webhook.half_open_max_calls",
		},
		"a circuit that can never close": {
			func(b *config.Breaker) {
				b.Webhook.HalfOpenMaxCalls = 2
				b.Webhook.HalfOpenSuccessThreshold = 3
			},
			"breaker.webhook.half_open_success_threshold",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := config.Defaults().Breaker
			tc.mutate(&b)
			wantProblem(t, check(&b), tc.field)
		})
	}
}

// A rate of exactly 1.0 means "only open when everything fails", which is
// unusual but not wrong.
func TestBreakerAcceptsAFullFailureThreshold(t *testing.T) {
	t.Parallel()
	b := config.Defaults().Breaker
	b.Webhook.FailureRateThreshold = 1
	b.Webhook.Window = config.Duration(time.Minute)

	wantNoProblem(t, check(&b))
}
