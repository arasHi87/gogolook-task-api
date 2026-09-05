package idempotency_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/idempotency"
)

// The fingerprint is what separates a retry from a mistake, so what changes it
// and what does not is the contract.
func TestFingerprint(t *testing.T) {
	t.Parallel()

	base := idempotency.Fingerprint("POST", "/tasks", []byte(`{"name":"a"}`))

	same := []struct {
		name         string
		method, path string
		body         string
	}{
		{"the identical request", "POST", "/tasks", `{"name":"a"}`},
		// The two surfaces are one endpoint served twice. A key used on one
		// and retried on the other is a retry, and telling that client it
		// reused its key would be refusing it for following the contract.
		{"the same request on the versioned surface", "POST", "/api/v1/tasks", `{"name":"a"}`},
	}
	for _, tc := range same {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := idempotency.Fingerprint(tc.method, tc.path, []byte(tc.body)); !bytes.Equal(got, base) {
				t.Errorf("fingerprint differs from the base request")
			}
		})
	}

	differ := []struct {
		name         string
		method, path string
		body         string
	}{
		{"a different body", "POST", "/tasks", `{"name":"b"}`},
		{"a different path", "POST", "/tasks/1", `{"name":"a"}`},
		{"a different method", "PUT", "/tasks", `{"name":"a"}`},
		{"an empty body", "POST", "/tasks", ""},
	}
	for _, tc := range differ {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := idempotency.Fingerprint(tc.method, tc.path, []byte(tc.body)); bytes.Equal(got, base) {
				t.Errorf("fingerprint matches the base request but the request is different")
			}
		})
	}
}

// The parts are length-prefixed rather than concatenated. Without that, a path
// and a body can be re-cut into a different path and body that hash the same,
// and one request replays another's response.
func TestFingerprintCannotBeReCut(t *testing.T) {
	t.Parallel()

	a := idempotency.Fingerprint("POST", "/tasks", []byte(`x{"name":"a"}`))
	b := idempotency.Fingerprint("POST", "/tasksx", []byte(`{"name":"a"}`))

	if bytes.Equal(a, b) {
		t.Error("moving a byte from the body to the path produced the same fingerprint")
	}
}

func TestCheckKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		key  string
		ok   bool
	}{
		{"a uuid", "9f1c2f8a-0b6d-4a1e-9c3f-2b7d5e8a4c11", true},
		{"any opaque string", "order-2026-09-05-0001", true},
		{"empty", "", false},
		{"too long", strings.Repeat("k", 256), false},
		// The key is a primary key we index and a value we log. Neither should
		// accept arbitrary bytes from a client.
		{"a control character", "abc\x00def", false},
		{"a newline, which would forge a log line", "abc\ndef", false},
		{"non-ascii", "ké", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := idempotency.CheckKey(tc.key, 255)
			if tc.ok && err != nil {
				t.Errorf("CheckKey(%q) = %v, want nil", tc.key, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("CheckKey(%q) = nil, want an error", tc.key)
			}
		})
	}
}
