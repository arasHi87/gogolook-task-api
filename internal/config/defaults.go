package config

import (
	"os"
	"time"
)

// Tier names. They are metric label values, so the set is closed.
const (
	TierAnonymous = "anonymous"
	TierStandard  = "standard"
	TierInternal  = "internal"
)

// Storage backends.
const (
	BackendMemory   = "memory"
	BackendPostgres = "postgres"
)

// Auth modes.
const (
	AuthOptional = "optional"
	AuthRequired = "required"
	AuthOff      = "off"
)

// Jitter strategies for the retry backoff.
const (
	JitterFull  = "full"
	JitterEqual = "equal"
	JitterNone  = "none"
)

// Demo credentials. The plaintext tokens are documented in .env.example and in
// the README so the rate-limit demo works out of the box; only their hashes
// live here. They are development credentials by construction — a deployment
// that cares replaces the clients list.
// G101 fires on these; it is wrong. They are SHA-256 digests, and the point of
// storing the hash rather than the token is that possessing it grants nothing.
//
//nolint:gosec // G101: hashes, not credentials
const (
	DemoStandardTokenSHA256 = "064462d4b78abaa304c5b4ab22e4a29119dbf18b3a8d7fc86e2fc633429f7dce"
	DemoInternalTokenSHA256 = "46f8c89e2172e93598e17b2a9e0261bb7848cfe389bf3d46bb98cc50c8b4710c"
)

// Defaults is the bottom layer of the precedence chain, and the configuration a
// bare `go run ./cmd/taskapi` uses: in-memory storage, optional auth, no
// Postgres, no Docker.
func Defaults() Config {
	return Config{
		Service: Service{
			Name:     "taskapi",
			Instance: defaultInstance(),
		},
		Logging: Logging{
			Level:  "info",
			Format: "auto",
		},
		HTTP: HTTP{
			Addr:              ":8080",
			ReadHeaderTimeout: Duration(5 * time.Second),
			ReadTimeout:       Duration(15 * time.Second),
			WriteTimeout:      Duration(30 * time.Second),
			IdleTimeout:       Duration(60 * time.Second),
			ShutdownGrace:     Duration(20 * time.Second),
			MaxBodyBytes:      1 << 20, // 1 MiB
			TrustedProxyHops:  0,
		},
		Admin: Admin{
			Addr:  ":9090",
			Pprof: true,
		},
		Storage: Storage{
			Backend: BackendMemory,
			Postgres: Postgres{
				DSN:                             "",
				MaxConns:                        20,
				MinConns:                        2,
				MaxConnLifetime:                 Duration(30 * time.Minute),
				MaxConnIdleTime:                 Duration(5 * time.Minute),
				HealthCheckPeriod:               Duration(30 * time.Second),
				ConnectTimeout:                  Duration(5 * time.Second),
				StatementTimeout:                Duration(5 * time.Second),
				IdleInTransactionSessionTimeout: Duration(10 * time.Second),
				LockTimeout:                     Duration(3 * time.Second),
			},
		},
		Queue: Queue{
			Workers:    8,
			ClaimBatch: 10,
			Lease:      Duration(30 * time.Second),
			// lease/3: two consecutive missed heartbeats are tolerated before
			// the reaper acts, the same reasoning as a lease TTL vs keepalive
			// interval in etcd or Raft.
			HeartbeatInterval: Duration(10 * time.Second),
			PollInterval:      Duration(2 * time.Second),
			ReaperInterval:    Duration(15 * time.Second),
			FetchCooldown:     Duration(100 * time.Millisecond),
			JobTimeout:        Duration(1 * time.Minute),
			MaxAttempts:       5,
			Backoff: Backoff{
				Base:   Duration(1 * time.Second),
				Max:    Duration(5 * time.Minute),
				Jitter: JitterFull,
			},
			Retention: Retention{
				Succeeded:     Duration(24 * time.Hour),
				Discarded:     Duration(7 * 24 * time.Hour),
				PurgeInterval: Duration(1 * time.Hour),
			},
		},
		Auth: Auth{
			Mode:  AuthOptional,
			Realm: "taskapi",
			Clients: []AuthClient{
				{ID: "demo", Tier: TierStandard, TokenSHA256: DemoStandardTokenSHA256},
				{ID: "worker", Tier: TierInternal, TokenSHA256: DemoInternalTokenSHA256},
			},
		},
		RateLimit: RateLimit{
			Enabled:        true,
			GlobalInflight: 200,
			KeyTTL:         Duration(10 * time.Minute),
			SweepInterval:  Duration(1 * time.Minute),
			Tiers: Tiers{
				Anonymous: Quota{Rate: 10, Burst: 20},
				Standard:  Quota{Rate: 100, Burst: 200},
				Internal:  Quota{Rate: 1000, Burst: 2000},
			},
		},
		Breaker: Breaker{
			Webhook: BreakerSettings{
				FailureRateThreshold:     0.5,
				MinThroughput:            20,
				Window:                   Duration(10 * time.Second),
				OpenDuration:             Duration(30 * time.Second),
				HalfOpenMaxCalls:         5,
				HalfOpenSuccessThreshold: 3,
			},
		},
		Webhook: Webhook{
			URL:     "",
			Timeout: Duration(3 * time.Second),
			Retry: WebhookRetry{
				MaxAttempts: 3,
				Base:        Duration(200 * time.Millisecond),
				Max:         Duration(5 * time.Second),
			},
		},
		Observability: Observability{
			Metrics: Metrics{Enabled: true, NativeHistograms: true},
			Tracing: Tracing{Enabled: false, OTLPEndpoint: "", SampleRatio: 0.1},
		},
	}
}

// defaultInstance names this replica. It shows up in application_name, in the
// queue's locked_by column and in log records, which is what makes "which pod
// was holding that lease" answerable.
func defaultInstance() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}
