package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/logging"
)

// newTestLogger returns a JSON logger writing into a buffer, with throttling
// off so every record is observable.
func newTestLogger(t *testing.T, level string) (*logging.Handle, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	h, err := logging.New(logging.Options{
		Level:      level,
		Format:     "json",
		Service:    "taskapi",
		Version:    "test",
		Writer:     &buf,
		NoThrottle: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h, &buf
}

// records decodes the buffer into one map per emitted line.
func records(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestParseLevelRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want slog.Level
		name string
	}{
		{"trace", logging.LevelTrace, "TRACE"},
		{"TRACE", logging.LevelTrace, "TRACE"},
		{"debug", slog.LevelDebug, "DEBUG"},
		{"info", slog.LevelInfo, "INFO"},
		{"", slog.LevelInfo, "INFO"},
		{"warn", slog.LevelWarn, "WARN"},
		{"warning", slog.LevelWarn, "WARN"},
		{"error", slog.LevelError, "ERROR"},
		{" Error ", slog.LevelError, "ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := logging.ParseLevel(tc.in)
			if err != nil {
				t.Fatalf("ParseLevel(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseLevel(%q) = %v, want %v", tc.in, got, tc.want)
			}
			if name := logging.LevelName(got); name != tc.name {
				t.Errorf("LevelName(%v) = %q, want %q", got, name, tc.name)
			}
		})
	}

	if _, err := logging.ParseLevel("chatty"); err == nil {
		t.Error("ParseLevel(chatty) = nil error, want an error naming the valid tiers")
	}
}

// The three tiers must actually gate what they claim to gate; this is the
// contract the -v and --trace flags sell.
func TestTiersGateOutput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		level string
		want  []string
	}{
		{"info", []string{"info-line", "warn-line"}},
		{"debug", []string{"debug-line", "info-line", "warn-line"}},
		{"trace", []string{"trace-line", "debug-line", "info-line", "warn-line"}},
	}
	for _, tc := range cases {
		t.Run(tc.level, func(t *testing.T) {
			t.Parallel()
			h, buf := newTestLogger(t, tc.level)
			ctx := logging.Into(context.Background(), h.Logger)

			logging.Trace(ctx, "trace-line")
			h.Debug("debug-line")
			h.Info("info-line")
			h.Warn("warn-line")

			var got []string
			for _, r := range records(t, buf) {
				msg, ok := r["msg"].(string)
				if !ok {
					t.Fatalf("record has no msg: %v", r)
				}
				got = append(got, msg)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("level %s emitted %v, want %v", tc.level, got, tc.want)
			}
		})
	}
}

func TestTraceRendersByName(t *testing.T) {
	t.Parallel()
	h, buf := newTestLogger(t, "trace")
	logging.Trace(logging.Into(context.Background(), h.Logger), "wire")

	got := records(t, buf)
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	if got[0]["level"] != "TRACE" {
		t.Errorf("level = %v, want TRACE (not the raw DEBUG-4 rendering)", got[0]["level"])
	}
}

func TestSetLevelIsLiveForDerivedLoggers(t *testing.T) {
	t.Parallel()
	h, buf := newTestLogger(t, "info")
	child := h.With("component", "queue")

	child.Debug("before")
	if n := len(records(t, buf)); n != 0 {
		t.Fatalf("got %d records at info, want 0", n)
	}

	if err := h.SetLevel("debug"); err != nil {
		t.Fatalf("SetLevel: %v", err)
	}
	child.Debug("after")

	got := records(t, buf)
	if len(got) != 1 || got[0]["msg"] != "after" {
		t.Errorf("after SetLevel(debug) got %v, want one 'after' record", got)
	}
	if h.LevelString() != "DEBUG" {
		t.Errorf("LevelString = %q, want DEBUG", h.LevelString())
	}
}

func TestEveryRecordCarriesServiceAndVersion(t *testing.T) {
	t.Parallel()
	h, buf := newTestLogger(t, "info")
	h.Info("boot")

	got := records(t, buf)[0]
	if got["service"] != "taskapi" || got["version"] != "test" {
		t.Errorf("record = %v, want service=taskapi version=test on every line", got)
	}
}

func TestContextLoggerCarriesScope(t *testing.T) {
	t.Parallel()
	h, buf := newTestLogger(t, "info")

	ctx := logging.Into(context.Background(), h.Logger)
	ctx = logging.With(ctx, slog.String("request_id", "req-1"))
	ctx = logging.With(ctx, slog.Int64("job_id", 7))
	logging.From(ctx).Info("scoped")

	got := records(t, buf)[0]
	if got["request_id"] != "req-1" {
		t.Errorf("request_id = %v, want req-1", got["request_id"])
	}
	if got["job_id"] != float64(7) {
		t.Errorf("job_id = %v, want 7 (With must not drop what the caller already attached)", got["job_id"])
	}
}

func TestFromContextNeverReturnsNil(t *testing.T) {
	t.Parallel()
	if logging.From(context.Background()) == nil {
		t.Error("From(empty ctx) = nil, want a usable logger")
	}
	//nolint:staticcheck // deliberately passing a nil context to prove it is safe
	if logging.From(nil) == nil {
		t.Error("From(nil) = nil, want a usable logger")
	}
}

func TestBadLevelStillYieldsUsableLogger(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	h, err := logging.New(logging.Options{Level: "loud", Format: "json", Writer: &buf})
	if err == nil {
		t.Error("New with a bad level returned no error")
	}
	if h == nil {
		t.Fatal("New returned a nil handle; a misconfigured process must still be able to say why")
	}
	h.Info("still works")
	if len(records(t, &buf)) != 1 {
		t.Error("fallback logger emitted nothing")
	}
}

func TestFingerprintIsStableAndShort(t *testing.T) {
	t.Parallel()
	a := logging.Fingerprint("super-secret-token")
	if len(a) != 8 {
		t.Errorf("Fingerprint length = %d, want 8", len(a))
	}
	if a != logging.Fingerprint("super-secret-token") {
		t.Error("Fingerprint is not stable")
	}
	if a == logging.Fingerprint("other-token") {
		t.Error("Fingerprint collided on distinct inputs")
	}
	if strings.Contains(a, "secret") {
		t.Error("Fingerprint leaked its input")
	}
}

func TestErrorAttrIsScrubbed(t *testing.T) {
	t.Parallel()
	h, buf := newTestLogger(t, "info")
	err := errors.New(`dial postgres://taskapi:hunter2@db:5432/tasks: connection refused`)
	h.Error("db unreachable", slog.Any("err", err))

	if s := buf.String(); strings.Contains(s, "hunter2") {
		t.Errorf("error text leaked a password: %s", s)
	}
}

// Both a bad level and a bad format must be reported, not just whichever was
// checked last.
func TestNewReportsEveryProblem(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h, err := logging.New(logging.Options{Level: "loud", Format: "interpretive dance", Writer: &buf})
	if err == nil {
		t.Fatal("New accepted both a bad level and a bad format")
	}
	for _, want := range []string{"loud", "interpretive dance"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q:\n%v", want, err)
		}
	}
	if h == nil {
		t.Fatal("New returned a nil handle")
	}
	h.Info("still works")
	if len(records(t, &buf)) != 1 {
		t.Error("the fallback logger emitted nothing")
	}
}

// The level vocabulary is duplicated in internal/config, which must stay a leaf
// package. This is the test that holds the two lists together.
func TestLevelVocabularyMatchesConfig(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		config.LevelError, config.LevelWarn, config.LevelInfo,
		config.LevelDebug, config.LevelTrace,
	} {
		if _, err := logging.ParseLevel(name); err != nil {
			t.Errorf("config accepts level %q but logging rejects it: %v", name, err)
		}
	}
	for _, name := range []string{
		config.FormatAuto, config.FormatText, config.FormatJSON,
	} {
		if _, err := logging.New(logging.Options{Format: name, Writer: io.Discard}); err != nil {
			t.Errorf("config accepts format %q but logging rejects it: %v", name, err)
		}
	}
}
