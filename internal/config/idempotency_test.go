package config_test

import (
	"testing"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

func TestIdempotencyDefaults(t *testing.T) {
	t.Parallel()
	i := config.Defaults().Idempotency

	switch {
	case !i.Enabled:
		t.Error("idempotency is off by default; a retried write would execute twice")
	case i.Required:
		t.Error("a key is required by default; the assignment's contract does not send one")
	case i.TTL.D() != 24*time.Hour:
		t.Errorf("ttl = %s, want 24h", i.TTL)
	}
	wantNoProblem(t, check(&i))
}

func TestIdempotencyValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(*config.Idempotency)
		field  string
	}{
		{
			name:   "ttl must be positive",
			mutate: func(i *config.Idempotency) { i.TTL = 0 },
			field:  "idempotency.ttl",
		},
		{
			name:   "purge interval must be positive",
			mutate: func(i *config.Idempotency) { i.PurgeInterval = 0 },
			field:  "idempotency.purge_interval",
		},
		{
			name:   "a key must be allowed at least one byte",
			mutate: func(i *config.Idempotency) { i.MaxKeyBytes = 0 },
			field:  "idempotency.max_key_bytes",
		},
		{
			// A btree entry stops fitting on a page well before this.
			name:   "a key cannot be unbounded",
			mutate: func(i *config.Idempotency) { i.MaxKeyBytes = 1 << 20 },
			field:  "idempotency.max_key_bytes",
		},
		{
			// Expired keys that outlive the purge replay a response the client
			// was told it could no longer rely on.
			name:   "purging cannot be rarer than the ttl",
			mutate: func(i *config.Idempotency) { i.PurgeInterval = i.TTL + 1 },
			field:  "idempotency.purge_interval",
		},
		{
			name:   "a key cannot be required while the layer is off",
			mutate: func(i *config.Idempotency) { i.Enabled, i.Required = false, true },
			field:  "idempotency.required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			i := config.Defaults().Idempotency
			tc.mutate(&i)
			wantProblem(t, check(&i), tc.field)
		})
	}
}
