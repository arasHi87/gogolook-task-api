package logging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"regexp"
	"strings"
)

// denied is the attribute-key denylist. Matching is exact and case-insensitive
// rather than substring-based, because substrings produce both false positives
// ("auth_mode", "unique_key", "page_token" are not secrets) and a false sense
// of coverage. Anything genuinely secret gets an entry here and a test.
var denied = map[string]struct{}{
	"access_token":    {},
	"api_key":         {},
	"apikey":          {},
	"auth_token":      {},
	"authorization":   {},
	"bearer":          {},
	"bearer_token":    {},
	"body":            {},
	"client_secret":   {},
	"cookie":          {},
	"credential":      {},
	"credentials":     {},
	"database_url":    {},
	"dsn":             {},
	"idempotency_key": {},
	"passwd":          {},
	"password":        {},
	"private_key":     {},
	"refresh_token":   {},
	"request_body":    {},
	"response_body":   {},
	"secret":          {},
	"set-cookie":      {},
	"token":           {},
}

// Value-level scrubbers. These are the backstop for the accidental case: a
// connection string logged under an innocent key such as "conn" or "target",
// or an error string that quotes the URL it failed to dial.
var (
	// scheme://user:password@host -> scheme://user:«redacted»@host
	urlUserinfoRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^:/@\s]+):[^@\s]*@`)
	// libpq keyword/value DSNs: password=hunter2
	kvPasswordRe = regexp.MustCompile(`(?i)\b(password|passwd|secret|token)\s*=\s*("[^"]*"|'[^']*'|\S+)`)
	// Authorization header values.
	bearerRe = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=\-]+`)
)

// ScrubValue applies the value-level scrubbers to a string.
func ScrubValue(s string) string {
	if s == "" {
		return s
	}
	if strings.Contains(s, "@") {
		s = urlUserinfoRe.ReplaceAllString(s, "$1:"+Redacted+"@")
	}
	if strings.ContainsAny(s, "=") {
		s = kvPasswordRe.ReplaceAllString(s, "$1="+Redacted)
	}
	s = bearerRe.ReplaceAllString(s, "$1 "+Redacted)
	return s
}

// Fingerprint is what you log instead of a secret you still need to correlate:
// the first 8 hex characters of its SHA-256. Enough to match two occurrences,
// useless to an attacker who reads the log.
func Fingerprint(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// redactHandler enforces the denylist for every backend handler. Doing this as
// a wrapper rather than as slog.HandlerOptions.ReplaceAttr matters: not every
// handler implementation honours ReplaceAttr, and redaction must not depend on
// which one is configured.
type redactHandler struct{ next slog.Handler }

// NewRedactHandler wraps h so that denylisted keys never reach it.
func NewRedactHandler(h slog.Handler) slog.Handler { return &redactHandler{next: h} }

func (h *redactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *redactHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, ScrubValue(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(sanitize(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h *redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		clean = append(clean, sanitize(a))
	}
	return &redactHandler{next: h.next.WithAttrs(clean)}
}

func (h *redactHandler) WithGroup(name string) slog.Handler {
	return &redactHandler{next: h.next.WithGroup(name)}
}

func sanitize(a slog.Attr) slog.Attr {
	if _, bad := denied[strings.ToLower(a.Key)]; bad {
		return slog.String(a.Key, Redacted)
	}

	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindGroup:
		attrs := v.Group()
		clean := make([]any, 0, len(attrs))
		for _, ga := range attrs {
			clean = append(clean, sanitize(ga))
		}
		return slog.Group(a.Key, clean...)
	case slog.KindString:
		if s := ScrubValue(v.String()); s != v.String() {
			return slog.String(a.Key, s)
		}
		return slog.Attr{Key: a.Key, Value: v}
	case slog.KindAny:
		// Errors routinely carry a DSN or a URL in their text.
		if err, ok := v.Any().(error); ok && err != nil {
			if s := ScrubValue(err.Error()); s != err.Error() {
				return slog.String(a.Key, s)
			}
		}
		return slog.Attr{Key: a.Key, Value: v}
	default:
		return slog.Attr{Key: a.Key, Value: v}
	}
}
