package config_test

import (
	"testing"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

// Every timeout has a default because its absence is a documented way to lose
// a server, and Go's own defaults are all "no limit".
func TestHTTPDefaultsSetEveryTimeout(t *testing.T) {
	t.Parallel()
	h := config.Defaults().HTTP

	timeouts := map[string]config.Duration{
		"read_header_timeout": h.ReadHeaderTimeout,
		"read_timeout":        h.ReadTimeout,
		"write_timeout":       h.WriteTimeout,
		"idle_timeout":        h.IdleTimeout,
		"shutdown_grace":      h.ShutdownGrace,
	}
	for name, d := range timeouts {
		if d <= 0 {
			t.Errorf("%s = %v, want a positive default", name, d)
		}
	}
	if h.Addr != ":8080" {
		t.Errorf("addr = %q, want :8080", h.Addr)
	}
	if h.MaxBodyBytes != 1<<20 {
		t.Errorf("max_body_bytes = %d, want 1 MiB", h.MaxBodyBytes)
	}
	// Trusting no forwarded hop is the only safe default: the leftmost
	// X-Forwarded-For entry is attacker-controlled.
	if h.TrustedProxyHops != 0 {
		t.Errorf("trusted_proxy_hops = %d, want 0", h.TrustedProxyHops)
	}
	wantNoProblem(t, check(&h))
}

func TestHTTPValidate(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		mutate func(*config.HTTP)
		field  string
	}{
		"empty addr":             {func(h *config.HTTP) { h.Addr = "" }, "http.addr"},
		"zero read timeout":      {func(h *config.HTTP) { h.ReadTimeout = 0 }, "http.read_timeout"},
		"negative write timeout": {func(h *config.HTTP) { h.WriteTimeout = config.Duration(-time.Second) }, "http.write_timeout"},
		"zero header timeout":    {func(h *config.HTTP) { h.ReadHeaderTimeout = 0 }, "http.read_header_timeout"},
		"zero idle timeout":      {func(h *config.HTTP) { h.IdleTimeout = 0 }, "http.idle_timeout"},
		"zero grace":             {func(h *config.HTTP) { h.ShutdownGrace = 0 }, "http.shutdown_grace"},
		"zero body cap":          {func(h *config.HTTP) { h.MaxBodyBytes = 0 }, "http.max_body_bytes"},
		"negative body cap":      {func(h *config.HTTP) { h.MaxBodyBytes = -1 }, "http.max_body_bytes"},
		"negative proxy hops":    {func(h *config.HTTP) { h.TrustedProxyHops = -1 }, "http.trusted_proxy_hops"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := config.Defaults().HTTP
			tc.mutate(&h)
			wantProblem(t, check(&h), tc.field)
		})
	}
}
