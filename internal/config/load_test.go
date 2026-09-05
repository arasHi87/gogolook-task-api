package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

// writeConfig drops a YAML file in a temp dir and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// newFlags returns a parsed flag set, as cobra would hand us.
func newFlags(t *testing.T, args ...string) *pflag.FlagSet {
	t.Helper()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	config.RegisterFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return fs
}

// environ turns a map into the []string shape os.Environ produces.
func environ(kv map[string]string) func() []string {
	out := make([]string, 0, len(kv))
	for k, v := range kv {
		out = append(out, k+"="+v)
	}
	return func() []string { return out }
}

func load(t *testing.T, o config.Options) *config.Result {
	t.Helper()
	res, err := config.Load(o)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return res
}

// The whole point of the section: flags beat environment beats file beats
// defaults. One case per layer, each overriding exactly the one below it.
func TestPrecedence(t *testing.T) {
	t.Parallel()

	file := writeConfig(t, `
http:
  addr: ":7000"
logging:
  level: warn
queue:
  workers: 3
storage:
  backend: memory
`)

	t.Run("defaults only", func(t *testing.T) {
		t.Parallel()
		got := load(t, config.Options{Environ: environ(nil)}).Config
		if got.HTTP.Addr != ":8080" {
			t.Errorf("http.addr = %q, want the built-in default :8080", got.HTTP.Addr)
		}
		if got.Storage.Backend != config.BackendMemory {
			t.Errorf("storage.backend = %q, want memory: `go run` must need no dependencies", got.Storage.Backend)
		}
	})

	t.Run("file overrides defaults", func(t *testing.T) {
		t.Parallel()
		got := load(t, config.Options{File: file, Environ: environ(nil)}).Config
		if got.HTTP.Addr != ":7000" {
			t.Errorf("http.addr = %q, want :7000 from the file", got.HTTP.Addr)
		}
		if got.Queue.Workers != 3 {
			t.Errorf("queue.workers = %d, want 3 from the file", got.Queue.Workers)
		}
		// Untouched keys keep their defaults rather than going to zero.
		if got.Admin.Addr != ":9090" {
			t.Errorf("admin.addr = %q, want the default :9090 to survive a partial file", got.Admin.Addr)
		}
	})

	t.Run("env overrides file", func(t *testing.T) {
		t.Parallel()
		got := load(t, config.Options{
			File:    file,
			Environ: environ(map[string]string{"TASKAPI_HTTP_ADDR": ":6000"}),
		}).Config
		if got.HTTP.Addr != ":6000" {
			t.Errorf("http.addr = %q, want :6000 from the environment", got.HTTP.Addr)
		}
		if got.Queue.Workers != 3 {
			t.Errorf("queue.workers = %d, want the file's 3 to survive an unrelated env var", got.Queue.Workers)
		}
	})

	t.Run("flag overrides env", func(t *testing.T) {
		t.Parallel()
		got := load(t, config.Options{
			File:    file,
			Environ: environ(map[string]string{"TASKAPI_HTTP_ADDR": ":6000"}),
			Flags:   newFlags(t, "--http.addr=:5000"),
		}).Config
		if got.HTTP.Addr != ":5000" {
			t.Errorf("http.addr = %q, want :5000 from the flag", got.HTTP.Addr)
		}
	})

	// The classic viper bug: merging flags that the user never set lets pflag's
	// defaults overwrite the file and the environment, inverting the whole
	// precedence chain. This is the test that catches it.
	t.Run("unset flags do not clobber lower layers", func(t *testing.T) {
		t.Parallel()
		got := load(t, config.Options{
			File:    file,
			Environ: environ(map[string]string{"TASKAPI_QUEUE_MAX_ATTEMPTS": "9"}),
			Flags:   newFlags(t, "--http.addr=:5000"),
		}).Config

		if got.Logging.Level != "warn" {
			t.Errorf("logging.level = %q, want the file's warn: an unset --logging.level must not overwrite it", got.Logging.Level)
		}
		if got.Queue.Workers != 3 {
			t.Errorf("queue.workers = %d, want the file's 3: an unset --queue.workers must not overwrite it", got.Queue.Workers)
		}
		if got.Queue.MaxAttempts != 9 {
			t.Errorf("queue.max_attempts = %d, want the environment's 9: an unset flag must not overwrite it", got.Queue.MaxAttempts)
		}
	})
}

// Three spellings, one knob. If this test passes, the documentation cannot lie
// about how to set something.
func TestThreeSpellingsAreOneKnob(t *testing.T) {
	t.Parallel()

	cases := []struct {
		path  string
		value string
		read  func(*config.Config) string
	}{
		{"http.addr", ":4321", func(c *config.Config) string { return c.HTTP.Addr }},
		{"logging.level", "debug", func(c *config.Config) string { return c.Logging.Level }},
		{"auth.mode", "required", func(c *config.Config) string { return c.Auth.Mode }},
		{"storage.backend", "postgres", func(c *config.Config) string { return c.Storage.Backend }},
		// A multi-word key: the naive "_ becomes ." rule produces
		// http.read.header.timeout and silently sets nothing.
		{"http.read_header_timeout", "9s", func(c *config.Config) string { return c.HTTP.ReadHeaderTimeout.String() }},
		{"queue.backoff.jitter", "equal", func(c *config.Config) string { return c.Queue.Backoff.Jitter }},
		{"storage.postgres.idle_in_transaction_session_timeout", "42s", func(c *config.Config) string {
			return c.Storage.Postgres.IdleInTransactionSessionTimeout.String()
		}},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()

			viaFile := load(t, config.Options{
				File:    writeConfig(t, yamlFor(tc.path, tc.value)),
				Environ: environ(nil),
			}).Config

			viaEnv := load(t, config.Options{
				Environ: environ(map[string]string{config.EnvName(tc.path): tc.value}),
			}).Config

			viaFlag := load(t, config.Options{
				Environ: environ(nil),
				Flags:   newFlags(t, "--set", tc.path+"="+tc.value),
			}).Config

			want := tc.value
			if got := tc.read(viaFile); got != want {
				t.Errorf("via config.yaml: %s = %q, want %q", tc.path, got, want)
			}
			if got := tc.read(viaEnv); got != want {
				t.Errorf("via %s: %s = %q, want %q", config.EnvName(tc.path), tc.path, got, want)
			}
			if got := tc.read(viaFlag); got != want {
				t.Errorf("via --set: %s = %q, want %q", tc.path, got, want)
			}
		})
	}
}

// yamlFor turns "a.b.c: v" into nested YAML.
func yamlFor(path, value string) string {
	parts := strings.Split(path, ".")
	var b strings.Builder
	for i, p := range parts {
		b.WriteString(strings.Repeat("  ", i))
		b.WriteString(p)
		b.WriteString(":")
		if i == len(parts)-1 {
			b.WriteString(" " + value)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func TestEnvNameRoundTrip(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"http.addr":                "TASKAPI_HTTP_ADDR",
		"http.read_header_timeout": "TASKAPI_HTTP_READ_HEADER_TIMEOUT",
		"storage.postgres.dsn":     "TASKAPI_STORAGE_POSTGRES_DSN",
		"queue.backoff.jitter":     "TASKAPI_QUEUE_BACKOFF_JITTER",
	}
	for path, want := range cases {
		if got := config.EnvName(path); got != want {
			t.Errorf("EnvName(%q) = %q, want %q", path, got, want)
		}
	}
}

// A typo'd environment variable that is silently ignored is an outage waiting
// to happen; it must be reported.
func TestUnknownEnvIsWarnedAbout(t *testing.T) {
	t.Parallel()
	res := load(t, config.Options{
		Environ: environ(map[string]string{
			"TASKAPI_HTTP_ADDRR": ":9999",
			"TASKAPI_HTTP_ADDR":  ":8888",
		}),
	})

	if res.Config.HTTP.Addr != ":8888" {
		t.Errorf("http.addr = %q, want :8888", res.Config.HTTP.Addr)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "TASKAPI_HTTP_ADDRR") {
		t.Errorf("warnings = %v, want one naming TASKAPI_HTTP_ADDRR", res.Warnings)
	}
}

// A typo in the config file is fatal rather than ignored: the file is the
// layer a human edits by hand, so it is the layer most worth being strict in.
func TestUnknownFileKeyIsFatal(t *testing.T) {
	t.Parallel()
	_, err := config.Load(config.Options{
		File:    writeConfig(t, "http:\n  adr: \":7000\"\n"),
		Environ: environ(nil),
	})
	if err == nil {
		t.Fatal("Load accepted an unknown key; a typo'd config file must not start silently")
	}
	if !strings.Contains(err.Error(), "http.adr") {
		t.Errorf("error = %v, want it to name the offending key", err)
	}
}

func TestMissingExplicitConfigIsFatal(t *testing.T) {
	t.Parallel()
	_, err := config.Load(config.Options{File: "/nonexistent/config.yaml", Environ: environ(nil)})
	if err == nil {
		t.Fatal("an explicit --config that does not exist must be an error")
	}
}

func TestUnknownSetKeyIsFatal(t *testing.T) {
	t.Parallel()
	_, err := config.Load(config.Options{
		Environ: environ(nil),
		Flags:   newFlags(t, "--set", "queue.wokers=4"),
	})
	if err == nil || !strings.Contains(err.Error(), "queue.wokers") {
		t.Fatalf("err = %v, want an error naming the unknown --set key", err)
	}
}

// -v and --trace are shorthands, and an explicit --logging.level must win over
// them: a user who typed both meant the specific one.
func TestVerbosityShorthands(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args []string
		want string
	}{
		{[]string{}, "info"},
		{[]string{"-v"}, "debug"},
		{[]string{"--debug"}, "debug"},
		{[]string{"--trace"}, "trace"},
		{[]string{"-v", "--trace"}, "trace"},
		{[]string{"-v", "--logging.level=error"}, "error"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			got := load(t, config.Options{Environ: environ(nil), Flags: newFlags(t, tc.args...)}).Config
			if got.Logging.Level != tc.want {
				t.Errorf("logging.level = %q, want %q", got.Logging.Level, tc.want)
			}
		})
	}
}

// Environment variables are always strings; ints, bools, floats and durations
// all have to survive the trip.
func TestEnvTypeCoercion(t *testing.T) {
	t.Parallel()
	got := load(t, config.Options{
		Environ: environ(map[string]string{
			"TASKAPI_QUEUE_WORKERS":                  "16",
			"TASKAPI_ADMIN_PPROF":                    "false",
			"TASKAPI_RATELIMIT_TIERS_STANDARD_RATE":  "250.5",
			"TASKAPI_QUEUE_LEASE":                    "45s",
			"TASKAPI_HTTP_MAX_BODY_BYTES":            "2097152",
			"TASKAPI_BREAKER_WEBHOOK_MIN_THROUGHPUT": "50",
		}),
	}).Config

	if got.Queue.Workers != 16 {
		t.Errorf("queue.workers = %d, want 16", got.Queue.Workers)
	}
	if got.Admin.Pprof {
		t.Error("admin.pprof = true, want false")
	}
	if got.RateLimit.Tiers.Standard.Rate != 250.5 {
		t.Errorf("ratelimit.tiers.standard.rate = %v, want 250.5", got.RateLimit.Tiers.Standard.Rate)
	}
	if got.Queue.Lease.D() != 45*time.Second {
		t.Errorf("queue.lease = %v, want 45s", got.Queue.Lease)
	}
	if got.HTTP.MaxBodyBytes != 2<<20 {
		t.Errorf("http.max_body_bytes = %d, want 2097152", got.HTTP.MaxBodyBytes)
	}
	if got.Breaker.Webhook.MinThroughput != 50 {
		t.Errorf("breaker.webhook.min_throughput = %d, want 50", got.Breaker.Webhook.MinThroughput)
	}
}

// The client list is a slice, which is the shape most likely to break in a
// map-based merge. Replacing it wholesale is the wanted semantics.
func TestAuthClientsReplaceRatherThanMerge(t *testing.T) {
	t.Parallel()
	got := load(t, config.Options{
		File: writeConfig(t, `
auth:
  clients:
    - {id: only, tier: internal, token_sha256: "`+strings.Repeat("a", 64)+`"}
`),
		Environ: environ(nil),
	}).Config

	if len(got.Auth.Clients) != 1 || got.Auth.Clients[0].ID != "only" {
		t.Fatalf("auth.clients = %+v, want the file's single entry to replace the defaults", got.Auth.Clients)
	}
	if got.Auth.Clients[0].Tier != config.TierInternal {
		t.Errorf("tier = %q, want internal", got.Auth.Clients[0].Tier)
	}
}
