package config

import (
	"fmt"
	"strings"

	"github.com/spf13/pflag"
)

// Flags that are not configuration paths. Everything else registered below is
// named exactly after its koanf path, which is what lets posflag map it with no
// translation table.
const (
	FlagConfig      = "config"
	FlagPrintConfig = "print-config"
	FlagDebug       = "debug"
	FlagTrace       = "trace"
	FlagSet         = "set"
)

// RegisterFlags declares the command-line surface on fs.
//
// The registered set is curated rather than exhaustive: every configuration key
// is reachable from the command line, but only the knobs an operator actually
// turns get a dedicated flag. Everything else goes through --set, so `--help`
// stays readable instead of listing sixty entries.
//
// All long flags are POSIX two-dash. Single-dash long flags do not parse; that
// is pflag's deliberate behaviour and it is documented in the README.
func RegisterFlags(fs *pflag.FlagSet) {
	d := Defaults()

	fs.String(FlagConfig, "", "path to config.yaml (default: ./config.yaml if present)")
	fs.Bool(FlagPrintConfig, false, "print the effective merged configuration as YAML and exit")
	fs.BoolP(FlagDebug, "v", false, "shorthand for --logging.level=debug")
	fs.Bool(FlagTrace, false, "shorthand for --logging.level=trace (logs SQL and outbound HTTP)")
	fs.StringArray(FlagSet, nil, "set any configuration key: --set queue.workers=16 (repeatable)")

	fs.String("logging.level", d.Logging.Level, "log level: error|warn|info|debug|trace")
	fs.String("logging.format", d.Logging.Format, "log format: auto|text|json")

	fs.String("http.addr", d.HTTP.Addr, "public listener address")
	fs.Duration("http.shutdown_grace", d.HTTP.ShutdownGrace.D(), "how long to drain in-flight work on SIGTERM")
	fs.Int("http.trusted_proxy_hops", d.HTTP.TrustedProxyHops, "X-Forwarded-For entries to trust, counted from the right")

	fs.String("admin.addr", d.Admin.Addr, "admin listener address (metrics, health, pprof)")

	fs.String("storage.backend", d.Storage.Backend, "storage backend: memory|postgres")
	fs.String("storage.postgres.dsn", d.Storage.Postgres.DSN, "Postgres DSN (prefer "+EnvName("storage.postgres.dsn")+")")

	fs.Int("queue.workers", d.Queue.Workers, "number of queue worker goroutines")
	fs.Int("queue.max_attempts", d.Queue.MaxAttempts, "attempts before a job is discarded to the dead-letter state")

	fs.String("auth.mode", d.Auth.Mode, "auth mode: optional|required|off")
	fs.Bool("ratelimit.enabled", d.RateLimit.Enabled, "enable inbound rate limiting")

	fs.String("webhook.url", d.Webhook.URL, "webhook endpoint task events are delivered to")
}

// flagOverrides turns the convenience flags into configuration paths. They are
// applied after posflag so they win, which is correct: they are flags.
func flagOverrides(fs *pflag.FlagSet, valid keyIndex) (map[string]any, error) {
	out := map[string]any{}

	// --trace beats -v beats an explicit --logging.level only if the explicit
	// one was not given; a user who typed both meant the specific one.
	if !fs.Changed("logging.level") {
		switch {
		case changed(fs, FlagTrace):
			out["logging.level"] = "trace"
		case changed(fs, FlagDebug):
			out["logging.level"] = "debug"
		}
	}

	for _, kv := range stringArray(fs, FlagSet) {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("--set %q: expected key=value", kv)
		}
		key = strings.TrimSpace(key)
		if _, known := valid.paths[key]; !known {
			return nil, fmt.Errorf("--set %s: unknown configuration key", key)
		}
		out[key] = value
	}
	return out, nil
}

func changed(fs *pflag.FlagSet, name string) bool {
	f := fs.Lookup(name)
	return f != nil && f.Changed
}

func stringArray(fs *pflag.FlagSet, name string) []string {
	if fs.Lookup(name) == nil {
		return nil
	}
	v, err := fs.GetStringArray(name)
	if err != nil {
		return nil
	}
	return v
}

// FilePath extracts --config from a parsed flag set.
func FilePath(fs *pflag.FlagSet) string {
	if fs == nil || fs.Lookup(FlagConfig) == nil {
		return ""
	}
	v, _ := fs.GetString(FlagConfig)
	return v
}

// PrintConfigRequested reports whether --print-config was given.
func PrintConfigRequested(fs *pflag.FlagSet) bool {
	if fs == nil || fs.Lookup(FlagPrintConfig) == nil {
		return false
	}
	v, _ := fs.GetBool(FlagPrintConfig)
	return v
}
