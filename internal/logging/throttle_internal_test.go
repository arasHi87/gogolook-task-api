package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// newFakeClockThrottle builds a throttle handler over a JSON backend with a
// clock the test drives, so suppression is asserted deterministically rather
// than by sleeping.
func newFakeClockThrottle(window time.Duration) (slog.Handler, *bytes.Buffer, *time.Time) {
	var buf bytes.Buffer
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	backend := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: LevelTrace})
	h := &throttleHandler{
		next:   backend,
		window: window,
		now:    func() time.Time { return now },
		seen:   make(map[string]*throttleEntry),
	}
	return h, &buf, &now
}

func lines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("decode %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

func emit(h slog.Handler, level slog.Level, msg string) {
	_ = h.Handle(context.Background(), slog.NewRecord(time.Now(), level, msg, 0))
}

// The contract: first occurrence immediately, then at most one per window,
// carrying a count of what was dropped in between.
func TestThrottleEmitsFirstThenOncePerWindow(t *testing.T) {
	t.Parallel()
	h, buf, now := newFakeClockThrottle(10 * time.Second)

	for range 5 {
		emit(h, slog.LevelError, "database unreachable")
	}
	got := lines(t, buf)
	if len(got) != 1 {
		t.Fatalf("got %d lines within the window, want 1 (the first)", len(got))
	}
	if _, ok := got[0]["suppressed"]; ok {
		t.Error("the first line must not carry a suppressed count")
	}

	*now = now.Add(11 * time.Second)
	emit(h, slog.LevelError, "database unreachable")

	got = lines(t, buf)
	if len(got) != 2 {
		t.Fatalf("got %d lines after the window elapsed, want 2", len(got))
	}
	if got[1]["suppressed"] != float64(4) {
		t.Errorf("suppressed = %v, want 4 — the operator needs to know how many were dropped", got[1]["suppressed"])
	}

	// The counter resets after it is reported.
	*now = now.Add(11 * time.Second)
	emit(h, slog.LevelError, "database unreachable")
	got = lines(t, buf)
	if _, ok := got[2]["suppressed"]; ok {
		t.Errorf("suppressed count did not reset: %v", got[2])
	}
}

// Below WARN nothing is throttled: DEBUG and TRACE are opt-in already, and
// dropping them would make -v lie about what happened.
func TestThrottleLeavesDebugAndInfoAlone(t *testing.T) {
	t.Parallel()
	h, buf, _ := newFakeClockThrottle(10 * time.Second)

	for range 4 {
		emit(h, slog.LevelInfo, "job claimed")
		emit(h, slog.LevelDebug, "sql")
		emit(h, LevelTrace, "wire")
	}
	if n := len(lines(t, buf)); n != 12 {
		t.Errorf("got %d lines, want 12 — records below WARN must pass through untouched", n)
	}
}

func TestThrottleKeysOnLevelAndMessage(t *testing.T) {
	t.Parallel()
	h, buf, _ := newFakeClockThrottle(10 * time.Second)

	emit(h, slog.LevelWarn, "breaker open")
	emit(h, slog.LevelWarn, "breaker open")
	emit(h, slog.LevelWarn, "lease expired")
	emit(h, slog.LevelError, "breaker open") // same text, different level

	got := lines(t, buf)
	if len(got) != 3 {
		t.Fatalf("got %d lines, want 3 (distinct level/message pairs)", len(got))
	}
}

// Two loggers derived from the same handle differ only by scope attributes.
// They must share one budget, or attaching a request_id defeats suppression
// entirely — which is exactly when the flood happens.
func TestDerivedLoggersShareTheBudget(t *testing.T) {
	t.Parallel()
	h, buf, _ := newFakeClockThrottle(10 * time.Second)

	a := slog.New(h).With("request_id", "req-1")
	b := slog.New(h).With("request_id", "req-2")
	a.Error("database unreachable")
	b.Error("database unreachable")

	if n := len(lines(t, buf)); n != 1 {
		t.Errorf("got %d lines, want 1 — derived loggers must not each get their own budget", n)
	}
}

// The suppression map is keyed by message text. A message built from unbounded
// input must not be able to grow it without limit.
func TestThrottleMapIsBounded(t *testing.T) {
	t.Parallel()
	h, _, now := newFakeClockThrottle(time.Second)
	th, ok := h.(*throttleHandler)
	if !ok {
		t.Fatalf("newFakeClockThrottle returned %T, want *throttleHandler", h)
	}

	for i := range maxThrottleKeys * 3 {
		*now = now.Add(time.Millisecond)
		emit(h, slog.LevelWarn, "unbounded "+string(rune('a'+i%26))+strings.Repeat("x", i%7))
	}

	th.mu.Lock()
	size := len(th.seen)
	th.mu.Unlock()

	if size > maxThrottleKeys {
		t.Errorf("suppression map holds %d keys, want at most %d", size, maxThrottleKeys)
	}
}
