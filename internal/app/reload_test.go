package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/logging"
)

// newTestApp builds an app around a config file the test can rewrite between
// reloads.
func newTestApp(t *testing.T, body string, args ...string) (*App, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	config.RegisterFlags(fs)
	if err := fs.Parse(append([]string{"--config=" + path}, args...)); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	res, err := config.Load(config.Options{File: path, Flags: fs})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err := res.Config.Validate(); err != nil {
		t.Fatalf("validate config: %v", err)
	}

	log, err := logging.New(logging.Options{
		Level: res.Config.Logging.Level, Format: "json", Writer: os.Stderr,
	})
	if err != nil {
		t.Fatalf("logger: %v", err)
	}

	a, err := New(Options{Mode: ModeAll, Config: res.Config, ConfigFile: path, Flags: fs, Logger: log})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a, path
}

func rewrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
}

func TestReloadAppliesHotFields(t *testing.T) {
	a, path := newTestApp(t, "queue:\n  workers: 4\n")

	rewrite(t, path, "queue:\n  workers: 12\nlogging:\n  level: debug\n")
	got := a.Reload()

	if got.Err != nil {
		t.Fatalf("Reload: %v", got.Err)
	}
	if a.Config().Queue.Workers != 12 {
		t.Errorf("queue.workers = %d, want 12", a.Config().Queue.Workers)
	}
	if a.Logger().LevelString() != "DEBUG" {
		t.Errorf("log level = %q, want DEBUG: the level lives in a LevelVar the whole tree shares",
			a.Logger().LevelString())
	}
	if len(got.Applied) != 2 {
		t.Errorf("applied = %v, want two changes", got.Applied)
	}
}

// A SIGHUP that touches a load-time field must say so and skip it, rather than
// leaving the live config claiming a listener address the process is not on.
func TestReloadSkipsRestartOnlyFields(t *testing.T) {
	a, path := newTestApp(t, "http:\n  addr: \":8080\"\n")

	rewrite(t, path, "http:\n  addr: \":9999\"\nqueue:\n  workers: 6\n")
	got := a.Reload()

	if got.Err != nil {
		t.Fatalf("Reload: %v", got.Err)
	}
	if a.Config().HTTP.Addr != ":8080" {
		t.Errorf("http.addr = %q, want the running :8080", a.Config().HTTP.Addr)
	}
	if a.Config().Queue.Workers != 6 {
		t.Errorf("queue.workers = %d, want the hot field applied alongside the skipped one", a.Config().Queue.Workers)
	}
	if len(got.RestartOnly) != 1 || got.RestartOnly[0].Field != "http.addr" {
		t.Errorf("restart-only = %v, want exactly http.addr", got.RestartOnly)
	}
}

// Reload is all-or-nothing. A config that fails validation leaves the running
// one untouched: a half-applied config is worse than a stale one.
func TestReloadRejectsInvalidConfigWholesale(t *testing.T) {
	a, path := newTestApp(t, "queue:\n  workers: 4\n")
	before := a.Config()

	rewrite(t, path, "queue:\n  workers: 99\nauth:\n  mode: maybe\n")
	got := a.Reload()

	if got.Err == nil {
		t.Fatal("Reload accepted an invalid config")
	}
	if a.Config() != before {
		t.Error("the running config was replaced despite a rejected reload")
	}
	if a.Config().Queue.Workers != 4 {
		t.Errorf("queue.workers = %d, want the pre-reload 4: nothing may be applied from a rejected reload",
			a.Config().Queue.Workers)
	}
}

func TestReloadRejectsUnreadableConfig(t *testing.T) {
	a, path := newTestApp(t, "queue:\n  workers: 4\n")

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove config: %v", err)
	}
	if got := a.Reload(); got.Err == nil {
		t.Fatal("Reload accepted a missing config file")
	}
	if a.Config().Queue.Workers != 4 {
		t.Error("a failed read changed the running config")
	}
}

// The bug this guards against: reloading by re-reading only the file drops
// every value that came from a flag, so a process started with -v goes quiet
// on the first SIGHUP.
func TestReloadPreservesFlagPrecedence(t *testing.T) {
	a, path := newTestApp(t, "queue:\n  workers: 4\n", "-v", "--queue.workers=7")

	if a.Config().Logging.Level != "debug" || a.Config().Queue.Workers != 7 {
		t.Fatalf("setup: level=%q workers=%d, want debug/7", a.Config().Logging.Level, a.Config().Queue.Workers)
	}

	rewrite(t, path, "queue:\n  workers: 4\n  max_attempts: 9\n")
	if got := a.Reload(); got.Err != nil {
		t.Fatalf("Reload: %v", got.Err)
	}

	if a.Config().Logging.Level != "debug" {
		t.Errorf("logging.level = %q after reload, want debug: -v must survive a SIGHUP", a.Config().Logging.Level)
	}
	if a.Config().Queue.Workers != 7 {
		t.Errorf("queue.workers = %d after reload, want the flag's 7 to still win over the file's 4",
			a.Config().Queue.Workers)
	}
	if a.Config().Queue.MaxAttempts != 9 {
		t.Errorf("queue.max_attempts = %d, want the file's new 9 to be picked up", a.Config().Queue.MaxAttempts)
	}
}

func TestNewRejectsBadWiring(t *testing.T) {
	log, err := logging.New(logging.Options{Format: "json", Writer: os.Stderr})
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	cfg := config.Defaults()

	cases := map[string]Options{
		"no config": {Mode: ModeAll, Logger: log},
		"no logger": {Mode: ModeAll, Config: &cfg},
		"bad mode":  {Mode: Mode("sideways"), Config: &cfg, Logger: log},
	}
	for name, o := range cases {
		if _, err := New(o); err == nil {
			t.Errorf("New(%s) = nil error, want a refusal", name)
		} else if name == "bad mode" && !strings.Contains(err.Error(), "sideways") {
			t.Errorf("New(bad mode) = %v, want it to name the bad value", err)
		}
	}
}

func TestModePredicates(t *testing.T) {
	cases := []struct {
		mode             Mode
		serves, consumes bool
	}{
		{ModeServe, true, false},
		{ModeWorker, false, true},
		{ModeAll, true, true},
	}
	for _, tc := range cases {
		if got := tc.mode.Runs(); got != tc.serves {
			t.Errorf("%s.Runs() = %v, want %v", tc.mode, got, tc.serves)
		}
		if got := tc.mode.Consumes(); got != tc.consumes {
			t.Errorf("%s.Consumes() = %v, want %v", tc.mode, got, tc.consumes)
		}
	}
}
