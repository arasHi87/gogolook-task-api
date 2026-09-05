// Package config owns the one configuration struct and the four-layer merge
// that fills it.
//
// Precedence, highest first:
//
//	--flags  >  TASKAPI_* environment  >  config.yaml  >  built-in defaults
//
// Every knob is reachable by all three spellings and they are provably the same
// knob: --http.addr, TASKAPI_HTTP_ADDR and `http: {addr: ...}` all resolve to
// the koanf path "http.addr". See envTransform and its test.
//
// The configuration is organised as sections. Each section is one type in one
// file that owns everything about itself: its fields, its defaults, its
// validation rules and its path in the tree. Adding a knob means editing one
// file, and the compiler says so if a piece is missing.
package config

// Section is one self-describing configuration module.
//
// Implementations use pointer receivers on a struct embedded in Config, so
// SetDefaults and Validate act on the live value rather than on a copy.
type Section interface {
	// Path is the koanf path this section is rooted at, and the prefix its
	// validation problems are reported under. A section does not otherwise know
	// where it is mounted.
	Path() string

	// SetDefaults fills in the built-in values.
	//
	// It replaces the whole section, so it is only meaningful on a zero value.
	// Defaults is the only caller.
	SetDefaults()

	// Validate appends every problem it finds. Rules that need to see more than
	// one section live in validateAcrossSections instead.
	Validate(p *Problems)
}

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

// sections lists every section, in the order they appear in the struct.
//
// This is the one place the set is enumerated. A new section is added here and
// nowhere else: defaults, validation and the section tests all iterate it.
func (c *Config) sections() []Section {
	return []Section{
		&c.Service,
		&c.Logging,
		&c.HTTP,
		&c.Admin,
		&c.Storage,
		&c.Queue,
		&c.Auth,
		&c.RateLimit,
		&c.Breaker,
		&c.Webhook,
		&c.Observability,
	}
}

// Defaults is the bottom layer of the precedence chain, and the configuration a
// bare `go run ./cmd/taskapi` uses: in-memory storage, optional auth, no
// Postgres, no Docker.
func Defaults() Config {
	var c Config
	for _, s := range c.sections() {
		s.SetDefaults()
	}
	return c
}

// Validate reports every problem with the configuration, not just the first.
func (c *Config) Validate() error {
	var p Problems
	for _, s := range c.sections() {
		s.Validate(&p)
	}
	c.validateAcrossSections(&p)
	return p.Err()
}

// validateAcrossSections holds the rules no single section can check, because
// they relate two of them. Keeping them here, rather than letting one section
// reach into another, is what keeps each section independently testable.
func (c *Config) validateAcrossSections(p *Problems) {
	if c.Admin.Addr == c.HTTP.Addr {
		p.Add("admin.addr",
			"must differ from http.addr: pprof and metrics must never be reachable on the public listener")
	}

	// A breaker whose open window is shorter than one delivery attempt never
	// gets to fail fast: the probe outlives the window it was opened for.
	if c.Webhook.Timeout > 0 && c.Breaker.Webhook.OpenDuration > 0 &&
		c.Webhook.Timeout > c.Breaker.Webhook.OpenDuration {
		p.Add("webhook.timeout", "must not exceed breaker.webhook.open_duration")
	}
}
