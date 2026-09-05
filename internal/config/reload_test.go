package config_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

func TestDiffFindsNothingWhenNothingChanged(t *testing.T) {
	t.Parallel()
	a, b := config.Defaults(), config.Defaults()
	hot, cold, err := config.Diff(&a, &b)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(hot) != 0 || len(cold) != 0 {
		t.Errorf("Diff of identical configs = hot %v, restart-only %v; want none", hot, cold)
	}
}

// SIGHUP applies the hot set and loudly refuses the rest. Getting the split
// wrong in either direction is bad: an ignored change that looks applied, or a
// live config claiming a listener address the process is not actually on.
func TestDiffSplitsHotFromRestartOnly(t *testing.T) {
	t.Parallel()

	next := config.Defaults()
	next.Logging.Level = "debug"                                     // hot
	next.Queue.Workers = 32                                          // hot
	next.RateLimit.Tiers.Standard.Rate = 500                         // hot
	next.Breaker.Webhook.OpenDuration = config.Duration(time.Minute) // hot
	next.HTTP.Addr = ":9999"                                         // restart-only
	next.Storage.Backend = config.BackendPostgres                    // restart-only
	next.Logging.Format = "json"                                     // restart-only

	old := config.Defaults()
	hot, cold, err := config.Diff(&old, &next)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	// Diff returns changes sorted by field, so the reload log lines come out in
	// a stable order regardless of which knobs moved.
	assertFields(t, "hot", hot, []string{
		"breaker.webhook.open_duration",
		"logging.level",
		"queue.workers",
		"ratelimit.tiers.standard.rate",
	})
	assertFields(t, "restart-only", cold, []string{
		"http.addr",
		"logging.format",
		"storage.backend",
	})
}

func assertFields(t *testing.T, label string, got []config.Change, want []string) {
	t.Helper()
	var names []string
	for _, c := range got {
		names = append(names, c.Field)
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("%s changes = %v, want %v", label, names, want)
	}
}

// Durations must render as "30s" in the change log, not as a nanosecond count.
func TestChangeRendersDurationsReadably(t *testing.T) {
	t.Parallel()
	old := config.Defaults()
	next := config.Defaults()
	next.Queue.Backoff.Max = config.Duration(10 * time.Minute)

	hot, _, err := config.Diff(&old, &next)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(hot) != 1 {
		t.Fatalf("hot = %v, want one change", hot)
	}
	if got := hot[0].String(); got != "queue.backoff.max from=5m0s to=10m0s" {
		t.Errorf("Change.String() = %q, want a human-readable duration", got)
	}
}

// A reload takes the hot fields and leaves everything else exactly as the
// process started: the live config's http.addr must always name the socket
// that is actually open.
func TestApplyHotTakesOnlyTheHotSet(t *testing.T) {
	t.Parallel()

	old := config.Defaults()
	next := config.Defaults()
	next.Queue.Workers = 32
	next.Logging.Level = "trace"
	next.HTTP.Addr = ":9999"
	next.Storage.Backend = config.BackendPostgres
	next.Storage.Postgres.DSN = "postgres://x@y/z"

	merged, err := config.ApplyHot(&old, &next)
	if err != nil {
		t.Fatalf("ApplyHot: %v", err)
	}

	if merged.Queue.Workers != 32 {
		t.Errorf("queue.workers = %d, want the new 32", merged.Queue.Workers)
	}
	if merged.Logging.Level != "trace" {
		t.Errorf("logging.level = %q, want the new trace", merged.Logging.Level)
	}
	if merged.HTTP.Addr != ":8080" {
		t.Errorf("http.addr = %q, want the running :8080 to be preserved", merged.HTTP.Addr)
	}
	if merged.Storage.Backend != config.BackendMemory {
		t.Errorf("storage.backend = %q, want the running memory backend to be preserved", merged.Storage.Backend)
	}
	if err := merged.Validate(); err != nil {
		t.Errorf("merged config must still be valid: %v", err)
	}
}

// The hot set includes the client list, and it is a slice — the shape most
// likely to be mangled by a flatten/unflatten round trip.
func TestApplyHotRoundTripsSlices(t *testing.T) {
	t.Parallel()

	old := config.Defaults()
	next := config.Defaults()
	next.Auth.Clients = []config.AuthClient{
		{ID: "new", Tier: config.TierStandard, TokenSHA256: strings.Repeat("b", 64)},
	}

	merged, err := config.ApplyHot(&old, &next)
	if err != nil {
		t.Fatalf("ApplyHot: %v", err)
	}
	if len(merged.Auth.Clients) != 1 || merged.Auth.Clients[0].ID != "new" {
		t.Fatalf("auth.clients = %+v, want the single new client", merged.Auth.Clients)
	}
	if merged.Auth.Clients[0].TokenSHA256 != strings.Repeat("b", 64) {
		t.Error("the token hash did not survive the round trip")
	}
}

// Every entry in the hot list must be a real configuration path. A typo there
// means a field silently becomes restart-only.
func TestHotPrefixesAreRealPaths(t *testing.T) {
	t.Parallel()

	base := config.Defaults()
	for _, path := range []string{
		"logging.level", "queue.workers", "queue.backoff.base", "ratelimit.enabled",
		"breaker.webhook.window", "webhook.timeout", "webhook.retry.base",
		"auth.mode", "auth.clients", "queue.retention.succeeded",
	} {
		if !config.IsHot(path) {
			t.Errorf("IsHot(%q) = false, want true", path)
		}
	}
	for _, path := range []string{
		"http.addr", "admin.addr", "storage.backend", "storage.postgres.dsn",
		"logging.format", "observability.metrics.enabled", "queue.lease",
		"webhook.url", "service.name",
	} {
		if config.IsHot(path) {
			t.Errorf("IsHot(%q) = true, want false — changing it needs a restart", path)
		}
	}
	_ = base
}

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
