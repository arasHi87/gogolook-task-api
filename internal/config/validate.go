package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var sha256Hex = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// Validate reports every problem with the configuration, not just the first.
// A config file with four mistakes should be fixable in one pass, not five.
func (c *Config) Validate() error {
	var v validator

	v.notEmpty("service.name", c.Service.Name)
	v.notEmpty("service.instance", c.Service.Instance)

	if err := checkLevelName(c.Logging.Level); err != nil {
		v.add("logging.level", err.Error())
	}
	v.oneOf("logging.format", c.Logging.Format, "auto", "text", "json")

	v.notEmpty("http.addr", c.HTTP.Addr)
	v.positive("http.read_header_timeout", c.HTTP.ReadHeaderTimeout)
	v.positive("http.read_timeout", c.HTTP.ReadTimeout)
	v.positive("http.write_timeout", c.HTTP.WriteTimeout)
	v.positive("http.idle_timeout", c.HTTP.IdleTimeout)
	v.positive("http.shutdown_grace", c.HTTP.ShutdownGrace)
	if c.HTTP.MaxBodyBytes <= 0 {
		v.add("http.max_body_bytes", "must be greater than zero")
	}
	if c.HTTP.TrustedProxyHops < 0 {
		v.add("http.trusted_proxy_hops", "must not be negative")
	}

	v.notEmpty("admin.addr", c.Admin.Addr)
	if c.Admin.Addr == c.HTTP.Addr {
		v.add("admin.addr", "must differ from http.addr: pprof and metrics must never be reachable on the public listener")
	}

	v.oneOf("storage.backend", c.Storage.Backend, BackendMemory, BackendPostgres)
	if c.Storage.Backend == BackendPostgres {
		if c.Storage.Postgres.DSN == "" {
			v.add("storage.postgres.dsn", "required when storage.backend is postgres (set "+EnvName("storage.postgres.dsn")+")")
		}
		if c.Storage.Postgres.MaxConns <= 0 {
			v.add("storage.postgres.max_conns", "must be greater than zero")
		}
		if c.Storage.Postgres.MinConns < 0 {
			v.add("storage.postgres.min_conns", "must not be negative")
		}
		if c.Storage.Postgres.MinConns > c.Storage.Postgres.MaxConns {
			v.add("storage.postgres.min_conns", "must not exceed storage.postgres.max_conns")
		}
		v.positive("storage.postgres.connect_timeout", c.Storage.Postgres.ConnectTimeout)
		v.positive("storage.postgres.statement_timeout", c.Storage.Postgres.StatementTimeout)
	}

	c.validateQueue(&v)
	c.validateAuth(&v)
	c.validateRateLimit(&v)
	c.validateBreaker(&v)
	c.validateWebhook(&v)

	if c.Observability.Tracing.Enabled && c.Observability.Tracing.OTLPEndpoint == "" {
		v.add("observability.tracing.otlp_endpoint", "required when tracing is enabled")
	}
	if r := c.Observability.Tracing.SampleRatio; r < 0 || r > 1 {
		v.add("observability.tracing.sample_ratio", "must be between 0 and 1")
	}

	return v.err()
}

func (c *Config) validateQueue(v *validator) {
	q := c.Queue
	if q.Workers <= 0 {
		v.add("queue.workers", "must be greater than zero")
	}
	if q.ClaimBatch <= 0 {
		v.add("queue.claim_batch", "must be greater than zero")
	}
	v.positive("queue.lease", q.Lease)
	v.positive("queue.heartbeat_interval", q.HeartbeatInterval)
	v.positive("queue.poll_interval", q.PollInterval)
	v.positive("queue.reaper_interval", q.ReaperInterval)
	v.positive("queue.job_timeout", q.JobTimeout)
	if q.FetchCooldown < 0 {
		v.add("queue.fetch_cooldown", "must not be negative")
	}
	// Two missed heartbeats must still fit inside the lease, or a worker that
	// pauses for one GC cycle loses jobs it is actively running.
	if q.HeartbeatInterval > 0 && q.Lease > 0 && q.HeartbeatInterval.D()*3 > q.Lease.D() {
		v.add("queue.heartbeat_interval", fmt.Sprintf(
			"must be at most one third of queue.lease (%s), so two missed heartbeats are tolerated before the reaper acts",
			q.Lease))
	}
	if q.MaxAttempts < 1 {
		v.add("queue.max_attempts", "must be at least 1")
	}
	v.positive("queue.backoff.base", q.Backoff.Base)
	v.positive("queue.backoff.max", q.Backoff.Max)
	if q.Backoff.Max > 0 && q.Backoff.Base > q.Backoff.Max {
		v.add("queue.backoff.base", "must not exceed queue.backoff.max")
	}
	v.oneOf("queue.backoff.jitter", q.Backoff.Jitter, JitterFull, JitterEqual, JitterNone)
	v.positive("queue.retention.succeeded", q.Retention.Succeeded)
	v.positive("queue.retention.discarded", q.Retention.Discarded)
	v.positive("queue.retention.purge_interval", q.Retention.PurgeInterval)
}

func (c *Config) validateAuth(v *validator) {
	v.oneOf("auth.mode", c.Auth.Mode, AuthOptional, AuthRequired, AuthOff)
	v.notEmpty("auth.realm", c.Auth.Realm)

	seenID := map[string]int{}
	seenToken := map[string]string{}
	for i, cl := range c.Auth.Clients {
		at := fmt.Sprintf("auth.clients[%d]", i)
		if cl.ID == "" {
			v.add(at+".id", "must not be empty")
		} else if prev, dup := seenID[cl.ID]; dup {
			v.add(at+".id", fmt.Sprintf("duplicate client id %q (also at auth.clients[%d])", cl.ID, prev))
		} else {
			seenID[cl.ID] = i
		}

		v.oneOf(at+".tier", cl.Tier, TierAnonymous, TierStandard, TierInternal)

		switch {
		case cl.TokenSHA256 == "":
			v.add(at+".token_sha256", "must not be empty")
		case !sha256Hex.MatchString(cl.TokenSHA256):
			v.add(at+".token_sha256", "must be 64 hexadecimal characters: store the SHA-256 of the token, never the token itself")
		default:
			lower := strings.ToLower(cl.TokenSHA256)
			if prev, dup := seenToken[lower]; dup {
				v.add(at+".token_sha256", fmt.Sprintf("duplicate token, already used by client %q", prev))
			}
			seenToken[lower] = cl.ID
		}
	}

	if c.Auth.Mode == AuthRequired && len(c.Auth.Clients) == 0 {
		v.add("auth.clients", "auth.mode is required but no clients are configured: every request would be rejected")
	}
}

func (c *Config) validateRateLimit(v *validator) {
	if !c.RateLimit.Enabled {
		return
	}
	if c.RateLimit.GlobalInflight <= 0 {
		v.add("ratelimit.global_inflight", "must be greater than zero")
	}
	v.positive("ratelimit.key_ttl", c.RateLimit.KeyTTL)
	v.positive("ratelimit.sweep_interval", c.RateLimit.SweepInterval)

	for name, q := range map[string]Quota{
		TierAnonymous: c.RateLimit.Tiers.Anonymous,
		TierStandard:  c.RateLimit.Tiers.Standard,
		TierInternal:  c.RateLimit.Tiers.Internal,
	} {
		at := "ratelimit.tiers." + name
		if q.Rate <= 0 {
			v.add(at+".rate", "must be greater than zero")
		}
		if q.Burst < 1 {
			v.add(at+".burst", "must be at least 1")
		}
	}
}

func (c *Config) validateBreaker(v *validator) {
	b := c.Breaker.Webhook
	if b.FailureRateThreshold <= 0 || b.FailureRateThreshold > 1 {
		v.add("breaker.webhook.failure_rate_threshold", "must be in (0, 1]")
	}
	if b.MinThroughput < 1 {
		v.add("breaker.webhook.min_throughput", "must be at least 1: without a floor, one failure out of one request opens the circuit")
	}
	v.positive("breaker.webhook.window", b.Window)
	v.positive("breaker.webhook.open_duration", b.OpenDuration)
	if b.HalfOpenMaxCalls < 1 {
		v.add("breaker.webhook.half_open_max_calls", "must be at least 1")
	}
	if b.HalfOpenSuccessThreshold < 1 {
		v.add("breaker.webhook.half_open_success_threshold", "must be at least 1")
	}
	if b.HalfOpenMaxCalls >= 1 && b.HalfOpenSuccessThreshold > b.HalfOpenMaxCalls {
		v.add("breaker.webhook.half_open_success_threshold",
			"must not exceed breaker.webhook.half_open_max_calls, or the circuit can never close")
	}
}

func (c *Config) validateWebhook(v *validator) {
	if c.Webhook.URL != "" {
		u, err := url.Parse(c.Webhook.URL)
		switch {
		case err != nil:
			v.add("webhook.url", fmt.Sprintf("is not a valid URL: %v", err))
		case u.Scheme != "http" && u.Scheme != "https":
			v.add("webhook.url", fmt.Sprintf("scheme %q is not supported (want http or https)", u.Scheme))
		case u.Host == "":
			v.add("webhook.url", "is missing a host")
		}
	}
	v.positive("webhook.timeout", c.Webhook.Timeout)
	if c.Webhook.Retry.MaxAttempts < 1 {
		v.add("webhook.retry.max_attempts", "must be at least 1")
	}
	v.positive("webhook.retry.base", c.Webhook.Retry.Base)
	v.positive("webhook.retry.max", c.Webhook.Retry.Max)
	if c.Webhook.Retry.Base > c.Webhook.Retry.Max {
		v.add("webhook.retry.base", "must not exceed webhook.retry.max")
	}
	// A breaker whose open window is shorter than one delivery attempt never
	// gets to fail fast: the probe outlives the window it was opened for.
	if c.Webhook.Timeout > 0 && c.Breaker.Webhook.OpenDuration > 0 &&
		c.Webhook.Timeout.D() > c.Breaker.Webhook.OpenDuration.D() {
		v.add("webhook.timeout", "must not exceed breaker.webhook.open_duration")
	}
}

// validator accumulates field-scoped problems.
type validator struct{ problems []error }

func (v *validator) add(field, msg string) {
	v.problems = append(v.problems, fmt.Errorf("%s: %s", field, msg))
}

func (v *validator) notEmpty(field, value string) {
	if strings.TrimSpace(value) == "" {
		v.add(field, "must not be empty")
	}
}

func (v *validator) positive(field string, d Duration) {
	if d <= 0 {
		v.add(field, "must be greater than zero")
	}
}

func (v *validator) oneOf(field, value string, allowed ...string) {
	for _, a := range allowed {
		if value == a {
			return
		}
	}
	v.add(field, fmt.Sprintf("%q is not one of: %s", value, strings.Join(allowed, ", ")))
}

func (v *validator) err() error {
	if len(v.problems) == 0 {
		return nil
	}
	return fmt.Errorf("invalid configuration:\n  %w", joinIndented(v.problems))
}

func joinIndented(errs []error) error {
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	return errors.New(strings.Join(msgs, "\n  "))
}

// checkLevelName duplicates the level vocabulary rather than importing
// internal/logging, so config stays a leaf package with no internal imports.
// The two lists are held together by a test in internal/logging.
func checkLevelName(s string) error {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trace", "debug", "info", "warn", "warning", "error", "err":
		return nil
	default:
		return fmt.Errorf("%q is not one of: error, warn, info, debug, trace", s)
	}
}
