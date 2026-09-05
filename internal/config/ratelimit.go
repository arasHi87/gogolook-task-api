package config

import "time"

// Rate-limit tiers.
//
// The set is closed because the tier name becomes a metric label, and a label
// whose value set is not bounded by construction is an outage waiting to
// happen.
const (
	// TierAnonymous is every caller without a recognised token, keyed by IP.
	TierAnonymous = "anonymous"
	// TierStandard is a configured client.
	TierStandard = "standard"
	// TierInternal is our own workers and health probes.
	TierInternal = "internal"
)

// RateLimit protects the service from its callers.
//
// A rate limiter points the opposite way from a circuit breaker: this one
// guards against callers, the breaker guards against dependencies. They are not
// substitutes.
type RateLimit struct {
	Enabled bool `koanf:"enabled" yaml:"enabled" json:"enabled"`
	// GlobalInflight is the load-shedding semaphore, and it is what actually
	// keeps the process alive under overload. A rate limiter does not: 200
	// requests per second of ten-second requests is still 2000 goroutines.
	GlobalInflight int      `koanf:"global_inflight" yaml:"global_inflight" json:"global_inflight"`
	KeyTTL         Duration `koanf:"key_ttl"         yaml:"key_ttl"         json:"key_ttl"`
	SweepInterval  Duration `koanf:"sweep_interval"  yaml:"sweep_interval"  json:"sweep_interval"`
	Tiers          Tiers    `koanf:"tiers"           yaml:"tiers"           json:"tiers"`
}

// Tiers is a fixed three-value set rather than a map, so the tier label's
// cardinality is bounded by the type system.
type Tiers struct {
	Anonymous Quota `koanf:"anonymous" yaml:"anonymous" json:"anonymous"`
	Standard  Quota `koanf:"standard"  yaml:"standard"  json:"standard"`
	Internal  Quota `koanf:"internal"  yaml:"internal"  json:"internal"`
}

// Quota is a token-bucket setting: sustained rate plus burst depth.
type Quota struct {
	Rate  float64 `koanf:"rate"  yaml:"rate"  json:"rate"`
	Burst int     `koanf:"burst" yaml:"burst" json:"burst"`
}

// Path implements Section.
func (RateLimit) Path() string { return "ratelimit" }

// SetDefaults implements Section.
func (r *RateLimit) SetDefaults() {
	*r = RateLimit{
		Enabled:        true,
		GlobalInflight: 200,
		KeyTTL:         Duration(10 * time.Minute),
		SweepInterval:  Duration(1 * time.Minute),
		Tiers: Tiers{
			Anonymous: Quota{Rate: 10, Burst: 20},
			Standard:  Quota{Rate: 100, Burst: 200},
			Internal:  Quota{Rate: 1000, Burst: 2000},
		},
	}
}

// Validate implements Section. A disabled limiter is not asked to justify its
// numbers, so turning it off for a benchmark does not require editing them.
func (r *RateLimit) Validate(p *Problems) {
	if !r.Enabled {
		return
	}
	at := func(f string) string { return join(r.Path(), f) }

	p.AtLeast(at("global_inflight"), r.GlobalInflight, 1)
	p.Positive(at("key_ttl"), r.KeyTTL)
	p.Positive(at("sweep_interval"), r.SweepInterval)

	r.Tiers.Anonymous.validate(p, at("tiers."+TierAnonymous))
	r.Tiers.Standard.validate(p, at("tiers."+TierStandard))
	r.Tiers.Internal.validate(p, at("tiers."+TierInternal))
}

func (q *Quota) validate(p *Problems, prefix string) {
	if q.Rate <= 0 {
		p.Add(join(prefix, "rate"), "must be greater than zero")
	}
	p.AtLeast(join(prefix, "burst"), q.Burst, 1)
}
