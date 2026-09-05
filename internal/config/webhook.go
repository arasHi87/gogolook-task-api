package config

import (
	"net/url"
	"time"
)

// Webhook is the outbound dependency task events are delivered to.
//
// It is the only genuinely remote thing this service talks to, which makes it
// the only honest place for a circuit breaker.
type Webhook struct {
	URL     string       `koanf:"url"     yaml:"url"     json:"url"`
	Timeout Duration     `koanf:"timeout" yaml:"timeout" json:"timeout"`
	Retry   WebhookRetry `koanf:"retry"   yaml:"retry"   json:"retry"`
}

// WebhookRetry is the per-delivery retry policy. Retries happen inside the
// breaker's accounting, so a retry storm actually trips it.
type WebhookRetry struct {
	MaxAttempts int      `koanf:"max_attempts" yaml:"max_attempts" json:"max_attempts"`
	Base        Duration `koanf:"base"         yaml:"base"         json:"base"`
	Max         Duration `koanf:"max"          yaml:"max"          json:"max"`
	// BudgetMaxRate caps retries as a fraction of the calls already in flight.
	// Without a budget, a total outage costs the dependency max_attempts times
	// its normal traffic at exactly the moment it can least take it.
	BudgetMaxRate float64 `koanf:"budget_max_rate" yaml:"budget_max_rate" json:"budget_max_rate"`
}

// Path implements Section.
func (Webhook) Path() string { return "webhook" }

// SetDefaults implements Section. The URL is empty by default: delivery is
// opt-in, so a bare `go run` does not try to call anything.
func (w *Webhook) SetDefaults() {
	*w = Webhook{
		URL:     "",
		Timeout: Duration(3 * time.Second),
		Retry: WebhookRetry{
			MaxAttempts: 3,
			Base:        Duration(200 * time.Millisecond),
			Max:         Duration(5 * time.Second),
			// One retry for every five calls in flight. An outage then costs
			// the dependency 1.2x its traffic rather than 3x.
			BudgetMaxRate: 0.2,
		},
	}
}

// Validate implements Section.
func (w *Webhook) Validate(p *Problems) {
	at := func(f string) string { return join(w.Path(), f) }

	if w.URL != "" {
		checkHTTPURL(p, at("url"), w.URL)
	}
	p.Positive(at("timeout"), w.Timeout)
	w.Retry.validate(p, at("retry"))
}

func (r *WebhookRetry) validate(p *Problems, prefix string) {
	at := func(f string) string { return join(prefix, f) }

	p.AtLeast(at("max_attempts"), r.MaxAttempts, 1)
	p.Positive(at("base"), r.Base)
	p.Positive(at("max"), r.Max)
	if r.Base > r.Max {
		p.Add(at("base"), "must not exceed %s", at("max"))
	}
	if r.BudgetMaxRate <= 0 || r.BudgetMaxRate > 1 {
		p.Add(at("budget_max_rate"), "must be in (0, 1]")
	}
}

func checkHTTPURL(p *Problems, field, raw string) {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		p.Add(field, "is not a valid URL: %v", err)
	case u.Scheme != "http" && u.Scheme != "https":
		p.Add(field, "scheme %q is not supported (want http or https)", u.Scheme)
	case u.Host == "":
		p.Add(field, "is missing a host")
	}
}
