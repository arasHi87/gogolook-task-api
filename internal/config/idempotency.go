package config

import "time"

// Idempotency is the Idempotency-Key layer on the write endpoints.
//
// It is the producer half of at-least-once: the queue guarantees an event is
// delivered at least once, and this guarantees a request that is *sent* more
// than once — a timeout the client could not distinguish from a failure, a
// retrying proxy — is *executed* only once.
type Idempotency struct {
	Enabled bool `koanf:"enabled" yaml:"enabled" json:"enabled"`
	// Required rejects a write that arrives without a key. Off by default:
	// the assignment's contract has no such header, and a service that 400s a
	// conforming request is not more correct, it is broken.
	Required bool `koanf:"required" yaml:"required" json:"required"`
	// TTL is how long a key is remembered. The draft leaves it to the server;
	// 24 hours is what Stripe and PayPal use, and it comfortably outlives any
	// retry a client or a proxy will still be making.
	TTL Duration `koanf:"ttl" yaml:"ttl" json:"ttl"`
	// PurgeInterval is how often expired keys are deleted. The table is small
	// and the index is on expires_at, so this is cheap and rare.
	PurgeInterval Duration `koanf:"purge_interval" yaml:"purge_interval" json:"purge_interval"`
	// MaxKeyBytes bounds what a client may send. The key is a primary key in a
	// table we own, so an unbounded one is an unbounded write.
	MaxKeyBytes int `koanf:"max_key_bytes" yaml:"max_key_bytes" json:"max_key_bytes"`
}

// Path implements Section.
func (Idempotency) Path() string { return "idempotency" }

// SetDefaults implements Section.
func (i *Idempotency) SetDefaults() {
	*i = Idempotency{
		Enabled:  true,
		Required: false,
		TTL:      Duration(24 * time.Hour),
		// An hour, not a minute: expired rows cost a little disk and nothing
		// else, and a purge that runs constantly is a write amplifier on a
		// table whose whole job is to be read.
		PurgeInterval: Duration(1 * time.Hour),
		MaxKeyBytes:   255,
	}
}

// Validate implements Section.
func (i *Idempotency) Validate(p *Problems) {
	at := func(f string) string { return join(i.Path(), f) }

	p.Positive(at("ttl"), i.TTL)
	p.Positive(at("purge_interval"), i.PurgeInterval)
	p.AtLeast(at("max_key_bytes"), i.MaxKeyBytes, 1)

	// The column is text with no length limit, but the row is a primary key
	// and 8 KiB is where a btree entry stops fitting on a page.
	if i.MaxKeyBytes > 8192 {
		p.Add(at("max_key_bytes"), "must not exceed 8192: a longer key does not fit a btree entry")
	}

	// Purging less often than the TTL means expired keys outlive their
	// promise, and a key that is expired but still present replays a response
	// the client was told it could no longer rely on.
	if i.TTL > 0 && i.PurgeInterval > i.TTL {
		p.Add(at("purge_interval"), "must not exceed %s (%s)", at("ttl"), i.TTL)
	}

	if i.Required && !i.Enabled {
		p.Add(at("required"), "cannot be set while %s is false", at("enabled"))
	}
}
