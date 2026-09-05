package config

import "time"

// Breaker holds one settings block per outbound target.
//
// One breaker per target, never a global one: a slow analytics endpoint must
// not be able to trip the circuit protecting the payment one.
type Breaker struct {
	Webhook BreakerSettings `koanf:"webhook" yaml:"webhook" json:"webhook"`
}

// BreakerSettings are the circuit-breaker knobs. The defaults are the values
// Resilience4j, Envoy and Polly converged on, and each one is defensible.
type BreakerSettings struct {
	// FailureRateThreshold of 0.5 is the near-universal default. Below about
	// half, the circuit trips on normal noise.
	FailureRateThreshold float64 `koanf:"failure_rate_threshold" yaml:"failure_rate_threshold" json:"failure_rate_threshold"`
	// MinThroughput is the floor below which the rate is not evaluated at all.
	// Without it, one failure out of one request opens the circuit — the single
	// most common misconfiguration of a breaker.
	MinThroughput uint `koanf:"min_throughput" yaml:"min_throughput" json:"min_throughput"`
	// Window is the rolling evaluation period.
	Window Duration `koanf:"window" yaml:"window" json:"window"`
	// OpenDuration is how long the circuit stays open before probing. Long
	// enough for a restart or a failover.
	OpenDuration Duration `koanf:"open_duration" yaml:"open_duration" json:"open_duration"`
	// HalfOpenMaxCalls probes without re-flooding a recovering dependency.
	HalfOpenMaxCalls uint `koanf:"half_open_max_calls" yaml:"half_open_max_calls" json:"half_open_max_calls"`
	// HalfOpenSuccessThreshold is how many probes must succeed to close. One
	// lucky success is not recovery.
	HalfOpenSuccessThreshold uint `koanf:"half_open_success_threshold" yaml:"half_open_success_threshold" json:"half_open_success_threshold"`
}

// Path implements Section.
func (Breaker) Path() string { return "breaker" }

// SetDefaults implements Section.
func (b *Breaker) SetDefaults() {
	*b = Breaker{
		Webhook: BreakerSettings{
			FailureRateThreshold:     0.5,
			MinThroughput:            20,
			Window:                   Duration(10 * time.Second),
			OpenDuration:             Duration(30 * time.Second),
			HalfOpenMaxCalls:         5,
			HalfOpenSuccessThreshold: 3,
		},
	}
}

// Validate implements Section.
func (b *Breaker) Validate(p *Problems) {
	b.Webhook.validate(p, join(b.Path(), "webhook"))
}

func (s *BreakerSettings) validate(p *Problems, prefix string) {
	at := func(f string) string { return join(prefix, f) }

	if s.FailureRateThreshold <= 0 || s.FailureRateThreshold > 1 {
		p.Add(at("failure_rate_threshold"), "must be in (0, 1]")
	}
	if s.MinThroughput < 1 {
		p.Add(at("min_throughput"),
			"must be at least 1: without a floor, one failure out of one request opens the circuit")
	}
	p.Positive(at("window"), s.Window)
	p.Positive(at("open_duration"), s.OpenDuration)

	if s.HalfOpenMaxCalls < 1 {
		p.Add(at("half_open_max_calls"), "must be at least 1")
	}
	if s.HalfOpenSuccessThreshold < 1 {
		p.Add(at("half_open_success_threshold"), "must be at least 1")
	}
	if s.HalfOpenMaxCalls >= 1 && s.HalfOpenSuccessThreshold > s.HalfOpenMaxCalls {
		p.Add(at("half_open_success_threshold"),
			"must not exceed %s, or the circuit can never close", at("half_open_max_calls"))
	}
}
