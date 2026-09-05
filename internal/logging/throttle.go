package logging

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// maxThrottleKeys bounds the suppression map. The key space is (level, message)
// and messages are compile-time constants in our code, so this is generous; the
// cap exists so a message built from user input can never grow it without limit.
const maxThrottleKeys = 512

// throttleHandler collapses repeated identical WARN and ERROR records. A
// breaker that flaps or a database that is down produces the same line
// thousands of times a second, and that flood is what hides the six lines that
// actually explain the incident.
//
// The contract: the first occurrence is emitted immediately, then at most one
// per window, carrying suppressed=<n> for the ones dropped in between. Records
// below WARN are never throttled — DEBUG and TRACE are opt-in already.
type throttleHandler struct {
	next   slog.Handler
	window time.Duration
	now    func() time.Time

	mu   sync.Mutex
	seen map[string]*throttleEntry
}

type throttleEntry struct {
	lastEmit   time.Time
	suppressed int64
}

// NewThrottleHandler wraps h, suppressing repeats of identical WARN/ERROR
// records to one per window.
func NewThrottleHandler(h slog.Handler, window time.Duration) slog.Handler {
	return &throttleHandler{
		next:   h,
		window: window,
		now:    time.Now,
		seen:   make(map[string]*throttleEntry),
	}
}

func (h *throttleHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *throttleHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < slog.LevelWarn {
		return h.next.Handle(ctx, r)
	}

	emit, suppressed := h.admit(r)
	if !emit {
		return nil
	}
	if suppressed > 0 {
		r.AddAttrs(slog.Int64("suppressed", suppressed))
	}
	return h.next.Handle(ctx, r)
}

// admit decides whether this record is emitted, and how many of its twins were
// dropped since the last emission.
func (h *throttleHandler) admit(r slog.Record) (emit bool, suppressed int64) {
	key := strconv.Itoa(int(r.Level)) + "\x00" + r.Message
	now := h.now()

	h.mu.Lock()
	defer h.mu.Unlock()

	e, ok := h.seen[key]
	if !ok {
		h.evictLocked(now)
		h.seen[key] = &throttleEntry{lastEmit: now}
		return true, 0
	}
	if now.Sub(e.lastEmit) >= h.window {
		n := e.suppressed
		e.suppressed = 0
		e.lastEmit = now
		return true, n
	}
	e.suppressed++
	return false, 0
}

// evictLocked drops entries that have gone quiet, but only when the map has
// grown past its cap — the common case does no work at all.
func (h *throttleHandler) evictLocked(now time.Time) {
	if len(h.seen) < maxThrottleKeys {
		return
	}
	cutoff := now.Add(-10 * h.window)
	for k, e := range h.seen {
		if e.lastEmit.Before(cutoff) {
			delete(h.seen, k)
		}
	}
	// Still full: the window is long relative to the churn. Start over rather
	// than grow without bound; the cost is one duplicated line per key.
	if len(h.seen) >= maxThrottleKeys {
		h.seen = make(map[string]*throttleEntry, maxThrottleKeys)
	}
}

func (h *throttleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h.derive(h.next.WithAttrs(attrs))
}

func (h *throttleHandler) WithGroup(name string) slog.Handler {
	return h.derive(h.next.WithGroup(name))
}

// derive shares the suppression state with the parent. Two loggers that differ
// only by a request_id attribute must not each get their own budget for the
// same "database unreachable" line.
func (h *throttleHandler) derive(next slog.Handler) slog.Handler {
	return &throttleShare{parent: h, next: next}
}

// throttleShare is a throttleHandler view that delegates admission to the
// handler it was derived from.
type throttleShare struct {
	parent *throttleHandler
	next   slog.Handler
}

func (h *throttleShare) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *throttleShare) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < slog.LevelWarn {
		return h.next.Handle(ctx, r)
	}
	emit, suppressed := h.parent.admit(r)
	if !emit {
		return nil
	}
	if suppressed > 0 {
		r.AddAttrs(slog.Int64("suppressed", suppressed))
	}
	return h.next.Handle(ctx, r)
}

func (h *throttleShare) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &throttleShare{parent: h.parent, next: h.next.WithAttrs(attrs)}
}

func (h *throttleShare) WithGroup(name string) slog.Handler {
	return &throttleShare{parent: h.parent, next: h.next.WithGroup(name)}
}
