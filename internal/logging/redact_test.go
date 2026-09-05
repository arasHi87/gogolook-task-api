package logging_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/logging"
)

// The rule from the design doc: bodies, DSNs and bearer tokens are never
// logged at any level. It is enforced by a denylist, not by discipline — so it
// gets a test that fails loudly if someone widens the surface.
func TestDenylistedKeysAreRedacted(t *testing.T) {
	t.Parallel()
	secrets := map[string]string{
		"password":        "hunter2",
		"dsn":             "postgres://taskapi:hunter2@db:5432/tasks",
		"token":           "sk_live_abc123",
		"authorization":   "Bearer sk_live_abc123",
		"api_key":         "ak_abc123",
		"idempotency_key": "1f0c1b2e-0000-4000-8000-000000000000",
		"cookie":          "session=abc123",
		"request_body":    `{"name":"buy milk"}`,
		"response_body":   `{"id":"abc"}`,
		"private_key":     "-----BEGIN PRIVATE KEY-----",
		"client_secret":   "cs_abc123",
	}

	for key, value := range secrets {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			h, buf := newTestLogger(t, "trace")
			h.Info("attempt", slog.String(key, value))

			got := records(t, buf)[0]
			if got[key] != logging.Redacted {
				t.Errorf("%s = %v, want %q", key, got[key], logging.Redacted)
			}
			if strings.Contains(buf.String(), value) {
				t.Errorf("secret leaked in output: %s", buf.String())
			}
		})
	}
}

// Redaction must not swallow the keys that merely look secret; over-redaction
// costs debuggability and trains people to turn it off.
func TestInnocentKeysSurvive(t *testing.T) {
	t.Parallel()
	h, buf := newTestLogger(t, "info")
	h.Info("list",
		slog.String("auth_mode", "optional"),
		slog.String("unique_key", "task.created:abc"),
		slog.String("page_token", "eyJpZCI6IjEifQ"),
		slog.String("client_id", "demo"),
		slog.String("token_sha256_prefix", "9f86d081"),
	)

	got := records(t, buf)[0]
	for k, want := range map[string]string{
		"auth_mode":           "optional",
		"unique_key":          "task.created:abc",
		"page_token":          "eyJpZCI6IjEifQ",
		"client_id":           "demo",
		"token_sha256_prefix": "9f86d081",
	} {
		if got[k] != want {
			t.Errorf("%s = %v, want %q — over-redaction costs debuggability", k, got[k], want)
		}
	}
}

// The value-level scrubbers are the backstop for the accidental case: a
// connection string logged under an innocent key.
func TestValueScrubbing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		in          string
		mustNotHave string
		mustHave    string
	}{
		{
			name:        "url userinfo",
			in:          "connecting to postgres://taskapi:hunter2@db:5432/tasks",
			mustNotHave: "hunter2",
			mustHave:    "postgres://taskapi:" + logging.Redacted + "@db:5432/tasks",
		},
		{
			name:        "libpq keyword dsn",
			in:          "host=db user=taskapi password=hunter2 sslmode=disable",
			mustNotHave: "hunter2",
			mustHave:    "password=" + logging.Redacted,
		},
		{
			name:        "bearer header",
			in:          "Authorization: Bearer sk_live_abc123",
			mustNotHave: "sk_live_abc123",
			mustHave:    "Bearer " + logging.Redacted,
		},
		{
			name:        "basic header",
			in:          "Basic dXNlcjpwYXNz",
			mustNotHave: "dXNlcjpwYXNz",
			mustHave:    "Basic " + logging.Redacted,
		},
		{
			name:        "nothing to scrub",
			in:          "listening on :8080",
			mustNotHave: logging.Redacted,
			mustHave:    "listening on :8080",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := logging.ScrubValue(tc.in)
			if !strings.Contains(got, tc.mustHave) {
				t.Errorf("ScrubValue(%q) = %q, want it to contain %q", tc.in, got, tc.mustHave)
			}
			if strings.Contains(got, tc.mustNotHave) {
				t.Errorf("ScrubValue(%q) = %q, must not contain %q", tc.in, got, tc.mustNotHave)
			}
		})
	}
}

// A secret smuggled inside a group, or attached with With() rather than at the
// call site, must be redacted too — both are paths that bypass a naive
// implementation that only sanitises the record's own attributes.
func TestRedactionCoversGroupsAndWith(t *testing.T) {
	t.Parallel()

	t.Run("group", func(t *testing.T) {
		t.Parallel()
		h, buf := newTestLogger(t, "info")
		h.Info("db", slog.Group("storage", slog.String("dsn", "postgres://u:p@h/db")))
		if strings.Contains(buf.String(), ":p@") {
			t.Errorf("group attribute leaked: %s", buf.String())
		}
	})

	t.Run("with", func(t *testing.T) {
		t.Parallel()
		h, buf := newTestLogger(t, "info")
		h.With("token", "sk_live_abc123").Info("call")
		if strings.Contains(buf.String(), "sk_live_abc123") {
			t.Errorf("With() attribute leaked: %s", buf.String())
		}
	})

	t.Run("message", func(t *testing.T) {
		t.Parallel()
		h, buf := newTestLogger(t, "info")
		h.Info("dialing postgres://u:hunter2@h/db")
		if strings.Contains(buf.String(), "hunter2") {
			t.Errorf("message text leaked: %s", buf.String())
		}
	})
}

func TestTextHandlerRedactsToo(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	h, err := logging.New(logging.Options{
		Level: "info", Format: "text", Writer: &buf, NoThrottle: true, TimeFormat: " ",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.Info("boot", slog.String("dsn", "postgres://u:hunter2@h/db"))

	if strings.Contains(buf.String(), "hunter2") {
		t.Errorf("text handler leaked a secret — redaction must not depend on the backend: %s", buf.String())
	}
}
