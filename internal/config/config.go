// Package config owns the one configuration struct and the four-layer merge
// that fills it.
//
// Precedence, highest first:
//
//	--flags  >  TASKAPI_* environment  >  config.yaml  >  built-in defaults
//
// Every knob is reachable by all three spellings and they are provably the same
// knob: --http.addr, TASKAPI_HTTP_ADDR and `http: {addr: ...}` all resolve to
// the koanf path "http.addr". See envToPath and its test.
package config

// Config is the whole configuration surface. Field tags are the koanf paths;
// they are also the flag names and (upper-cased, dots to underscores) the
// environment variable names.
type Config struct {
	Service       Service       `koanf:"service"       yaml:"service"       json:"service"`
	Logging       Logging       `koanf:"logging"       yaml:"logging"       json:"logging"`
	HTTP          HTTP          `koanf:"http"          yaml:"http"          json:"http"`
	Admin         Admin         `koanf:"admin"         yaml:"admin"         json:"admin"`
	Storage       Storage       `koanf:"storage"       yaml:"storage"       json:"storage"`
	Queue         Queue         `koanf:"queue"         yaml:"queue"         json:"queue"`
	Auth          Auth          `koanf:"auth"          yaml:"auth"          json:"auth"`
	RateLimit     RateLimit     `koanf:"ratelimit"     yaml:"ratelimit"     json:"ratelimit"`
	Breaker       Breaker       `koanf:"breaker"       yaml:"breaker"       json:"breaker"`
	Webhook       Webhook       `koanf:"webhook"       yaml:"webhook"       json:"webhook"`
	Observability Observability `koanf:"observability" yaml:"observability" json:"observability"`
}

// Service identifies the process in logs, metrics and pg_stat_activity.
type Service struct {
	Name string `koanf:"name" yaml:"name" json:"name"`
	// Instance defaults to the hostname; it disambiguates replicas in
	// application_name and in the queue's locked_by column.
	Instance string `koanf:"instance" yaml:"instance" json:"instance"`
}

// Logging maps to the three tiers in internal/logging.
type Logging struct {
	Level  string `koanf:"level"  yaml:"level"  json:"level"`  // error|warn|info|debug|trace
	Format string `koanf:"format" yaml:"format" json:"format"` // auto|text|json
}

// HTTP is the public listener. Every timeout here exists because its absence is
// a known way to lose a server: no ReadHeaderTimeout is a Slowloris, no
// WriteTimeout is a hung client holding a goroutine forever.
type HTTP struct {
	Addr              string   `koanf:"addr"                yaml:"addr"                json:"addr"`
	ReadHeaderTimeout Duration `koanf:"read_header_timeout" yaml:"read_header_timeout" json:"read_header_timeout"`
	ReadTimeout       Duration `koanf:"read_timeout"        yaml:"read_timeout"        json:"read_timeout"`
	WriteTimeout      Duration `koanf:"write_timeout"       yaml:"write_timeout"       json:"write_timeout"`
	IdleTimeout       Duration `koanf:"idle_timeout"        yaml:"idle_timeout"        json:"idle_timeout"`
	ShutdownGrace     Duration `koanf:"shutdown_grace"      yaml:"shutdown_grace"      json:"shutdown_grace"`
	MaxBodyBytes      int64    `koanf:"max_body_bytes"      yaml:"max_body_bytes"      json:"max_body_bytes"`
	// TrustedProxyHops is how many X-Forwarded-For entries to trust, counted
	// from the RIGHT. Zero means trust none and use the socket peer: the only
	// safe default, because the leftmost XFF entry is attacker-controlled.
	TrustedProxyHops int `koanf:"trusted_proxy_hops" yaml:"trusted_proxy_hops" json:"trusted_proxy_hops"`
}

// Admin is the private listener: metrics, health, pprof, debug endpoints. It is
// a separate port so none of that is ever reachable from the public one.
type Admin struct {
	Addr  string `koanf:"addr"  yaml:"addr"  json:"addr"`
	Pprof bool   `koanf:"pprof" yaml:"pprof" json:"pprof"`
}

// Storage selects the repository implementation.
type Storage struct {
	// Backend is memory or postgres. memory is the default so that `go run`
	// works with no dependencies at all.
	Backend  string   `koanf:"backend"  yaml:"backend"  json:"backend"`
	Postgres Postgres `koanf:"postgres" yaml:"postgres" json:"postgres"`
}

// Postgres holds the pool and session settings from the connection-health
// table. The DSN is never written to the YAML file and never logged.
type Postgres struct {
	DSN                             string   `koanf:"dsn"                                 yaml:"dsn"                                 json:"dsn"`
	MaxConns                        int32    `koanf:"max_conns"                           yaml:"max_conns"                           json:"max_conns"`
	MinConns                        int32    `koanf:"min_conns"                           yaml:"min_conns"                           json:"min_conns"`
	MaxConnLifetime                 Duration `koanf:"max_conn_lifetime"                   yaml:"max_conn_lifetime"                   json:"max_conn_lifetime"`
	MaxConnIdleTime                 Duration `koanf:"max_conn_idle_time"                  yaml:"max_conn_idle_time"                  json:"max_conn_idle_time"`
	HealthCheckPeriod               Duration `koanf:"health_check_period"                 yaml:"health_check_period"                 json:"health_check_period"`
	ConnectTimeout                  Duration `koanf:"connect_timeout"                     yaml:"connect_timeout"                     json:"connect_timeout"`
	StatementTimeout                Duration `koanf:"statement_timeout"                   yaml:"statement_timeout"                   json:"statement_timeout"`
	IdleInTransactionSessionTimeout Duration `koanf:"idle_in_transaction_session_timeout" yaml:"idle_in_transaction_session_timeout" json:"idle_in_transaction_session_timeout"`
	LockTimeout                     Duration `koanf:"lock_timeout"                        yaml:"lock_timeout"                        json:"lock_timeout"`
}

// Queue is the Postgres-backed job queue.
type Queue struct {
	Workers    int `koanf:"workers"     yaml:"workers"     json:"workers"`
	ClaimBatch int `koanf:"claim_batch" yaml:"claim_batch" json:"claim_batch"`
	// Lease is the visibility timeout: how long a claim is honoured without a
	// heartbeat before the reaper may reclaim the job.
	Lease             Duration `koanf:"lease"              yaml:"lease"              json:"lease"`
	HeartbeatInterval Duration `koanf:"heartbeat_interval" yaml:"heartbeat_interval" json:"heartbeat_interval"`
	PollInterval      Duration `koanf:"poll_interval"      yaml:"poll_interval"      json:"poll_interval"`
	ReaperInterval    Duration `koanf:"reaper_interval"    yaml:"reaper_interval"    json:"reaper_interval"`
	// FetchCooldown is the minimum gap between claims after a NOTIFY, so a
	// thousand-row insert burst does not become a thousand claim round-trips.
	FetchCooldown Duration  `koanf:"fetch_cooldown" yaml:"fetch_cooldown" json:"fetch_cooldown"`
	JobTimeout    Duration  `koanf:"job_timeout"    yaml:"job_timeout"    json:"job_timeout"`
	MaxAttempts   int       `koanf:"max_attempts"   yaml:"max_attempts"   json:"max_attempts"`
	Backoff       Backoff   `koanf:"backoff"        yaml:"backoff"        json:"backoff"`
	Retention     Retention `koanf:"retention"      yaml:"retention"      json:"retention"`
}

// Backoff is the retry schedule for failed jobs.
type Backoff struct {
	Base   Duration `koanf:"base"   yaml:"base"   json:"base"`
	Max    Duration `koanf:"max"    yaml:"max"    json:"max"`
	Jitter string   `koanf:"jitter" yaml:"jitter" json:"jitter"` // full|equal|none
}

// Retention governs the purge loop. A queue table that never deletes grows
// forever and takes its indexes with it.
type Retention struct {
	Succeeded     Duration `koanf:"succeeded"      yaml:"succeeded"      json:"succeeded"`
	Discarded     Duration `koanf:"discarded"      yaml:"discarded"      json:"discarded"`
	PurgeInterval Duration `koanf:"purge_interval" yaml:"purge_interval" json:"purge_interval"`
}

// Auth resolves a bearer token to a client id and a rate-limit tier. It is a
// quota dimension, not an authorisation system.
type Auth struct {
	// Mode is optional, required or off. optional is the default and never
	// returns 401: an unknown caller is simply the anonymous tier.
	Mode    string       `koanf:"mode"    yaml:"mode"    json:"mode"`
	Realm   string       `koanf:"realm"   yaml:"realm"   json:"realm"`
	Clients []AuthClient `koanf:"clients" yaml:"clients" json:"clients"`
}

// AuthClient is one configured caller. Only the hash is ever stored here.
type AuthClient struct {
	ID          string `koanf:"id"           yaml:"id"           json:"id"`
	Tier        string `koanf:"tier"         yaml:"tier"         json:"tier"`
	TokenSHA256 string `koanf:"token_sha256" yaml:"token_sha256" json:"token_sha256"`
}

// RateLimit protects the service from its callers.
type RateLimit struct {
	Enabled bool `koanf:"enabled" yaml:"enabled" json:"enabled"`
	// GlobalInflight is the load-shedding semaphore. This, not the rate limit,
	// is what keeps the process alive under overload.
	GlobalInflight int      `koanf:"global_inflight" yaml:"global_inflight" json:"global_inflight"`
	KeyTTL         Duration `koanf:"key_ttl"         yaml:"key_ttl"         json:"key_ttl"`
	SweepInterval  Duration `koanf:"sweep_interval"  yaml:"sweep_interval"  json:"sweep_interval"`
	Tiers          Tiers    `koanf:"tiers"           yaml:"tiers"           json:"tiers"`
}

// Tiers is a fixed three-value set rather than a map, because the tier name
// becomes a metric label and its cardinality must be bounded by construction.
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

// Breaker holds one settings block per outbound target. One breaker per target,
// never a global one — a slow analytics endpoint must not trip the payment one.
type Breaker struct {
	Webhook BreakerSettings `koanf:"webhook" yaml:"webhook" json:"webhook"`
}

// BreakerSettings are the circuit-breaker knobs.
type BreakerSettings struct {
	FailureRateThreshold float64 `koanf:"failure_rate_threshold" yaml:"failure_rate_threshold" json:"failure_rate_threshold"`
	// MinThroughput is the floor below which the rate is not evaluated at all.
	// Without it, one failure out of one request opens the circuit — the single
	// most common misconfiguration.
	MinThroughput            uint     `koanf:"min_throughput"              yaml:"min_throughput"              json:"min_throughput"`
	Window                   Duration `koanf:"window"                      yaml:"window"                      json:"window"`
	OpenDuration             Duration `koanf:"open_duration"               yaml:"open_duration"               json:"open_duration"`
	HalfOpenMaxCalls         uint     `koanf:"half_open_max_calls"         yaml:"half_open_max_calls"         json:"half_open_max_calls"`
	HalfOpenSuccessThreshold uint     `koanf:"half_open_success_threshold" yaml:"half_open_success_threshold" json:"half_open_success_threshold"`
}

// Webhook is the outbound dependency task events are delivered to.
type Webhook struct {
	URL     string       `koanf:"url"     yaml:"url"     json:"url"`
	Timeout Duration     `koanf:"timeout" yaml:"timeout" json:"timeout"`
	Retry   WebhookRetry `koanf:"retry"   yaml:"retry"   json:"retry"`
}

// WebhookRetry is the per-delivery retry policy, inside the breaker's
// accounting so a retry storm actually trips it.
type WebhookRetry struct {
	MaxAttempts int      `koanf:"max_attempts" yaml:"max_attempts" json:"max_attempts"`
	Base        Duration `koanf:"base"         yaml:"base"         json:"base"`
	Max         Duration `koanf:"max"          yaml:"max"          json:"max"`
}

// Observability configures the metrics registry and the tracing exporter.
type Observability struct {
	Metrics Metrics `koanf:"metrics" yaml:"metrics" json:"metrics"`
	Tracing Tracing `koanf:"tracing" yaml:"tracing" json:"tracing"`
}

// Metrics toggles the Prometheus surface.
type Metrics struct {
	Enabled bool `koanf:"enabled" yaml:"enabled" json:"enabled"`
	// NativeHistograms exposes native histograms alongside the classic buckets.
	NativeHistograms bool `koanf:"native_histograms" yaml:"native_histograms" json:"native_histograms"`
}

// Tracing configures OTLP export.
type Tracing struct {
	Enabled      bool    `koanf:"enabled"       yaml:"enabled"       json:"enabled"`
	OTLPEndpoint string  `koanf:"otlp_endpoint" yaml:"otlp_endpoint" json:"otlp_endpoint"`
	SampleRatio  float64 `koanf:"sample_ratio"  yaml:"sample_ratio"  json:"sample_ratio"`
}
