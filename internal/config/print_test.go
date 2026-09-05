package config_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

func TestPrintConfigRedactsTheDSN(t *testing.T) {
	t.Parallel()

	c := config.Defaults()
	c.Storage.Backend = config.BackendPostgres
	c.Storage.Postgres.DSN = "postgres://taskapi:hunter2@db:5432/tasks"

	var buf bytes.Buffer
	if err := c.WriteYAML(&buf); err != nil {
		t.Fatalf("WriteYAML: %v", err)
	}
	out := buf.String()

	if strings.Contains(out, "hunter2") {
		t.Errorf("--print-config leaked the DSN password:\n%s", out)
	}
	if !strings.Contains(out, config.RedactedValue) {
		t.Errorf("--print-config did not mark the DSN as redacted:\n%s", out)
	}
	// Durations must be readable, not nanosecond counts.
	if !strings.Contains(out, "read_header_timeout: 5s") {
		t.Errorf("--print-config rendered durations unreadably:\n%s", out)
	}
	// Redaction must not mutate the caller's config.
	if c.Storage.Postgres.DSN == config.RedactedValue {
		t.Error("WriteYAML mutated the live configuration")
	}
}

// Redacted is what both --print-config and GET /debug/config render, so there
// is one definition of "which fields are secret" and the two cannot disagree.
func TestRedactedMasksSecretsWithoutMutating(t *testing.T) {
	t.Parallel()

	c := config.Defaults()
	c.Storage.Postgres.DSN = "postgres://taskapi:hunter2@db:5432/tasks"

	got := c.Redacted()
	if got.Storage.Postgres.DSN != config.RedactedValue {
		t.Errorf("dsn = %q, want it masked", got.Storage.Postgres.DSN)
	}
	if c.Storage.Postgres.DSN == config.RedactedValue {
		t.Error("Redacted mutated the caller's configuration")
	}

	// A SHA-256 is not a credential, and showing it is how an operator
	// confirms which client list is loaded.
	if len(got.Auth.Clients) == 0 || got.Auth.Clients[0].TokenSHA256 != c.Auth.Clients[0].TokenSHA256 {
		t.Error("token hashes were masked; they are not secrets and hiding them costs debuggability")
	}
}

func TestRedactedLeavesAnEmptyDSNAlone(t *testing.T) {
	t.Parallel()

	c := config.Defaults()
	if got := c.Redacted().Storage.Postgres.DSN; got != "" {
		t.Errorf("dsn = %q, want empty: there is nothing to redact", got)
	}
}

func TestStringRendersRedactedYAML(t *testing.T) {
	t.Parallel()

	c := config.Defaults()
	c.Storage.Postgres.DSN = "postgres://u:hunter2@h/db"

	out := c.String()
	if strings.Contains(out, "hunter2") {
		t.Errorf("String() leaked the password:\n%s", out)
	}
	if !strings.Contains(out, "backend: memory") {
		t.Errorf("String() is not the effective config:\n%s", out)
	}
}

func TestNilConfigRedactsToNil(t *testing.T) {
	t.Parallel()

	var c *config.Config
	if c.Redacted() != nil {
		t.Error("Redacted() on a nil config returned something")
	}
}

// WriteYAML must produce something that loads again, or --print-config is not
// a usable starting point for a config file.
func TestPrintedConfigLoadsBack(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	c := config.Defaults()
	if err := c.WriteYAML(&buf); err != nil {
		t.Fatalf("WriteYAML: %v", err)
	}

	res := load(t, config.Options{File: writeConfig(t, buf.String()), Environ: environ(nil)})
	if res.Config.Queue.Lease != c.Queue.Lease {
		t.Errorf("queue.lease did not survive the round trip: %v vs %v", res.Config.Queue.Lease, c.Queue.Lease)
	}
	if res.Config.HTTP.Addr != c.HTTP.Addr {
		t.Errorf("http.addr did not survive the round trip: %q vs %q", res.Config.HTTP.Addr, c.HTTP.Addr)
	}
}
