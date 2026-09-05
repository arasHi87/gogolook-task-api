package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/cli"
	"github.com/arasHi87/gogolook-task-api/internal/config"
)

// run executes the command tree with args and captures stdout.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer
	root := cli.NewRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)

	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func TestVersion(t *testing.T) {
	t.Parallel()
	out, err := run(t, "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.HasPrefix(out, "taskapi ") || !strings.Contains(out, "commit") {
		t.Errorf("version output = %q, want a one-line build banner", out)
	}
}

func TestVersionJSON(t *testing.T) {
	t.Parallel()
	out, err := run(t, "version", "--json")
	if err != nil {
		t.Fatalf("version --json: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("version --json produced invalid JSON %q: %v", out, err)
	}
	for _, k := range []string{"version", "commit", "go_version", "platform"} {
		if got[k] == "" {
			t.Errorf("version --json is missing %s: %v", k, got)
		}
	}
}

// --print-config renders the effective merged configuration to stdout and
// exits 0 without starting anything.
func TestPrintConfig(t *testing.T) {
	t.Parallel()
	out, err := run(t, "all", "--print-config", "--queue.workers=13")
	if err != nil {
		t.Fatalf("--print-config: %v", err)
	}
	if !strings.Contains(out, "workers: 13") {
		t.Errorf("--print-config did not reflect the flag:\n%s", out)
	}
	if !strings.Contains(out, "backend: memory") {
		t.Errorf("--print-config is missing the storage backend:\n%s", out)
	}
}

func TestPrintConfigRedactsSecrets(t *testing.T) {
	t.Parallel()
	out, err := run(t, "all", "--print-config",
		"--storage.backend=postgres",
		"--storage.postgres.dsn=postgres://taskapi:hunter2@db:5432/tasks")
	if err != nil {
		t.Fatalf("--print-config: %v", err)
	}
	if strings.Contains(out, "hunter2") {
		t.Errorf("--print-config leaked the DSN password:\n%s", out)
	}
	if !strings.Contains(out, config.RedactedValue) {
		t.Errorf("--print-config did not mark the DSN redacted:\n%s", out)
	}
}

// Every problem is reported at once, so a bad config is fixed in one pass.
func TestInvalidConfigListsEveryProblem(t *testing.T) {
	t.Parallel()
	_, err := run(t, "all", "--queue.workers=0", "--auth.mode=maybe")
	if err == nil {
		t.Fatal("an invalid configuration must not start the process")
	}
	for _, want := range []string{"queue.workers", "auth.mode"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %s:\n%v", want, err)
		}
	}
}

// pflag is POSIX: long flags take two dashes. -config parses as the shorthand
// cluster -c -o -n..., which is not defined, so it fails loudly rather than
// being silently ignored. This is deliberate and documented.
func TestSingleDashLongFlagIsRejected(t *testing.T) {
	t.Parallel()
	if _, err := run(t, "all", "-print-config"); err == nil {
		t.Fatal("-print-config parsed; long flags must require two dashes")
	}
}

func TestUnknownFlagIsRejected(t *testing.T) {
	t.Parallel()
	if _, err := run(t, "all", "--nonsense"); err == nil {
		t.Fatal("an unknown flag must be an error")
	}
}

func TestSubcommandsTakeNoArguments(t *testing.T) {
	t.Parallel()
	for _, sub := range []string{"serve", "worker", "all", "version", "healthcheck"} {
		if _, err := run(t, sub, "stray-argument"); err == nil {
			t.Errorf("%s accepted a positional argument", sub)
		}
	}
}

// The healthcheck subcommand exists because the runtime image is distroless
// and has no curl. With nothing listening it must fail, not hang.
func TestHealthcheckFailsWhenNothingIsListening(t *testing.T) {
	t.Parallel()
	_, err := run(t, "healthcheck", "--admin.addr=127.0.0.1:1", "--timeout=200ms")
	if err == nil {
		t.Fatal("healthcheck reported success with nothing listening")
	}
}
