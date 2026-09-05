package config

import "time"

// Storage backends.
const (
	// BackendMemory is the default: the whole API with no dependencies at all.
	BackendMemory = "memory"
	// BackendPostgres adds durability, the transactional outbox and the queue.
	BackendPostgres = "postgres"
)

// Storage selects the repository implementation.
type Storage struct {
	Backend  string   `koanf:"backend"  yaml:"backend"  json:"backend"`
	Postgres Postgres `koanf:"postgres" yaml:"postgres" json:"postgres"`
}

// Postgres holds the pool and session settings.
//
// The DSN is never written to the YAML file and never logged; it comes from the
// environment or a secret file, and --print-config renders it redacted.
type Postgres struct {
	DSN               string   `koanf:"dsn"                 yaml:"dsn"                 json:"dsn"`
	MaxConns          int32    `koanf:"max_conns"           yaml:"max_conns"           json:"max_conns"`
	MinConns          int32    `koanf:"min_conns"           yaml:"min_conns"           json:"min_conns"`
	MaxConnLifetime   Duration `koanf:"max_conn_lifetime"   yaml:"max_conn_lifetime"   json:"max_conn_lifetime"`
	MaxConnIdleTime   Duration `koanf:"max_conn_idle_time"  yaml:"max_conn_idle_time"  json:"max_conn_idle_time"`
	HealthCheckPeriod Duration `koanf:"health_check_period" yaml:"health_check_period" json:"health_check_period"`
	ConnectTimeout    Duration `koanf:"connect_timeout"     yaml:"connect_timeout"     json:"connect_timeout"`
	// StatementTimeout stops a runaway query pinning a connection forever.
	StatementTimeout Duration `koanf:"statement_timeout" yaml:"statement_timeout" json:"statement_timeout"`
	// IdleInTransactionSessionTimeout stops a bug that leaves a transaction
	// open from blocking VACUUM indefinitely.
	IdleInTransactionSessionTimeout Duration `koanf:"idle_in_transaction_session_timeout" yaml:"idle_in_transaction_session_timeout" json:"idle_in_transaction_session_timeout"`
	LockTimeout                     Duration `koanf:"lock_timeout"                        yaml:"lock_timeout"                        json:"lock_timeout"`
}

// Path implements Section.
func (Storage) Path() string { return "storage" }

// SetDefaults implements Section.
func (s *Storage) SetDefaults() {
	*s = Storage{
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
	}
}

// Validate implements Section. The Postgres settings are only checked when that
// backend is actually selected, so a memory-backed run is not asked to justify
// a pool size it will never build.
func (s *Storage) Validate(p *Problems) {
	p.OneOf(join(s.Path(), "backend"), s.Backend, BackendMemory, BackendPostgres)
	if s.Backend == BackendPostgres {
		s.Postgres.validate(p, join(s.Path(), "postgres"))
	}
}

func (pg *Postgres) validate(p *Problems, prefix string) {
	at := func(f string) string { return join(prefix, f) }

	if pg.DSN == "" {
		p.Add(at("dsn"), "required when storage.backend is postgres (set %s)", EnvName("storage.postgres.dsn"))
	}
	if pg.MaxConns <= 0 {
		p.Add(at("max_conns"), "must be greater than zero")
	}
	if pg.MinConns < 0 {
		p.Add(at("min_conns"), "must not be negative")
	}
	if pg.MinConns > pg.MaxConns {
		p.Add(at("min_conns"), "must not exceed %s", at("max_conns"))
	}
	p.Positive(at("connect_timeout"), pg.ConnectTimeout)
	p.Positive(at("statement_timeout"), pg.StatementTimeout)
}
