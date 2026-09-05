package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

// check runs one section's own rules and returns what it reported.
//
// Sections validate themselves, so a section test never has to build a whole
// Config or reason about anything outside its own file.
func check(s config.Section) error {
	var p config.Problems
	s.Validate(&p)
	return p.Err()
}

// wantProblem asserts that a section refused something, and named the field.
func wantProblem(t *testing.T, err error, field string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Validate accepted the value; want a problem naming %q", field)
	}
	if !strings.Contains(err.Error(), field) {
		t.Errorf("Validate = %v, want it to name %q", err, field)
	}
}

// wantNoProblem asserts a section accepted the value.
func wantNoProblem(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Validate = %v, want nil", err)
	}
}

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

// load merges the layers and fails the test if that is not possible.
func load(t *testing.T, o config.Options) *config.Result {
	t.Helper()
	res, err := config.Load(o)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return res
}

// yamlFor turns "a.b.c" and a value into the nested YAML that sets it.
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
