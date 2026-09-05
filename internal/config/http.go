package config

import "time"

// HTTP is the public listener.
//
// Every timeout here exists because its absence is a documented way to lose a
// server: no read-header timeout is a Slowloris, no read timeout lets a slow
// body hold a goroutine indefinitely, no write timeout lets a stalled client do
// the same on the way out, and no idle timeout leaves keep-alive connections
// parked forever. Go's own defaults are all "no limit".
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

// Path implements Section.
func (HTTP) Path() string { return "http" }

// SetDefaults implements Section.
func (h *HTTP) SetDefaults() {
	*h = HTTP{
		Addr:              ":8080",
		ReadHeaderTimeout: Duration(5 * time.Second),
		ReadTimeout:       Duration(15 * time.Second),
		WriteTimeout:      Duration(30 * time.Second),
		IdleTimeout:       Duration(60 * time.Second),
		ShutdownGrace:     Duration(20 * time.Second),
		MaxBodyBytes:      1 << 20, // 1 MiB
		TrustedProxyHops:  0,
	}
}

// Validate implements Section.
func (h *HTTP) Validate(p *Problems) {
	at := func(f string) string { return join(h.Path(), f) }

	p.NotEmpty(at("addr"), h.Addr)
	p.Positive(at("read_header_timeout"), h.ReadHeaderTimeout)
	p.Positive(at("read_timeout"), h.ReadTimeout)
	p.Positive(at("write_timeout"), h.WriteTimeout)
	p.Positive(at("idle_timeout"), h.IdleTimeout)
	p.Positive(at("shutdown_grace"), h.ShutdownGrace)

	if h.MaxBodyBytes <= 0 {
		p.Add(at("max_body_bytes"), "must be greater than zero")
	}
	if h.TrustedProxyHops < 0 {
		p.Add(at("trusted_proxy_hops"), "must not be negative")
	}
}
