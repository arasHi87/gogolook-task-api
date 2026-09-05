package config_test

import (
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

func TestRateLimitDefaults(t *testing.T) {
	t.Parallel()
	r := config.Defaults().RateLimit

	if !r.Enabled {
		t.Error("enabled = false, want true")
	}
	// The in-flight semaphore is what actually keeps the process alive under
	// overload; a rate limit does not. 200 rps of ten-second requests is still
	// 2000 concurrent goroutines.
	if r.GlobalInflight <= 0 {
		t.Errorf("global_inflight = %d, want a positive load-shedding limit", r.GlobalInflight)
	}
	// Tiers must widen: an anonymous caller can never out-spend a known one.
	if !(r.Tiers.Anonymous.Rate < r.Tiers.Standard.Rate && r.Tiers.Standard.Rate < r.Tiers.Internal.Rate) {
		t.Errorf("tier rates do not widen: %v", r.Tiers)
	}
	wantNoProblem(t, check(&r))
}

func TestRateLimitValidate(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		mutate func(*config.RateLimit)
		field  string
	}{
		"no inflight budget": {func(r *config.RateLimit) { r.GlobalInflight = 0 }, "ratelimit.global_inflight"},
		"zero key ttl":       {func(r *config.RateLimit) { r.KeyTTL = 0 }, "ratelimit.key_ttl"},
		"zero sweep":         {func(r *config.RateLimit) { r.SweepInterval = 0 }, "ratelimit.sweep_interval"},
		"anonymous rate zero": {
			func(r *config.RateLimit) { r.Tiers.Anonymous.Rate = 0 },
			"ratelimit.tiers.anonymous.rate",
		},
		"standard burst zero": {
			func(r *config.RateLimit) { r.Tiers.Standard.Burst = 0 },
			"ratelimit.tiers.standard.burst",
		},
		"internal rate negative": {
			func(r *config.RateLimit) { r.Tiers.Internal.Rate = -1 },
			"ratelimit.tiers.internal.rate",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := config.Defaults().RateLimit
			tc.mutate(&r)
			wantProblem(t, check(&r), tc.field)
		})
	}
}

// Turning the limiter off for a benchmark must not require editing numbers it
// will never read.
func TestRateLimitDisabledSkipsItsOwnRules(t *testing.T) {
	t.Parallel()
	r := config.Defaults().RateLimit
	r.Enabled = false
	r.GlobalInflight = 0
	r.Tiers.Anonymous.Rate = 0
	r.Tiers.Standard.Burst = -1

	wantNoProblem(t, check(&r))
}
