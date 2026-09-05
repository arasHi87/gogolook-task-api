// Package logging sets up the process-wide slog logger.
//
// Three tiers, matching the convention used in our other Go services:
//
//	(none)    INFO+   the lifecycle narrative only — one line per state change
//	-v        DEBUG+  + per-request and per-job lifecycle
//	--trace   TRACE+  + the wire: SQL, outbound HTTP, pool churn, heartbeats
//
// TRACE is slog.Level(-8), the conventional "Debug - 4" slot on slog's numeric
// scale. Everything is written to stderr so stdout stays free for machine data
// (--print-config, migrate status --json).
package logging

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
)

// LevelTrace is the wire-level tier. slog has no TRACE, and -8 is the
// conventional slot four steps below DEBUG.
const LevelTrace = slog.Level(-8)

// Redacted is what a denylisted attribute renders as. It is deliberately
// distinctive so it is greppable in a log dump.
const Redacted = "«redacted»"

// Output formats.
const (
	FormatAuto = "auto"
	FormatText = "text"
	FormatJSON = "json"
)

// throttleWindow is how often an identical WARN or ERROR is allowed through.
const throttleWindow = 10 * time.Second

// Options configures New. The zero value is usable: it produces an INFO-level
// logger on stderr with the format chosen by TTY detection.
type Options struct {
	// Level is one of error, warn, info, debug, trace. Empty means info.
	Level string
	// Format is one of auto, text, json. Empty means auto: text when stderr is
	// a terminal, json otherwise.
	Format string
	// Service and Version are attached to every record.
	Service string
	Version string
	// Writer defaults to os.Stderr.
	Writer io.Writer
	// AddSource attaches file:line. Off by default: it costs a caller lookup on
	// every record and the message plus attrs are usually enough.
	AddSource bool
	// NoThrottle disables repeat suppression of identical WARN/ERROR lines.
	// Tests set it so they can assert on every emitted record.
	NoThrottle bool
	// TimeFormat overrides the text handler's timestamp layout. Tests set it to
	// "" together with a fixed writer to get stable golden output.
	TimeFormat string
}

// Handle owns the logger and the level knob behind it. The level is a single
// *slog.LevelVar so it can be moved at runtime by flag, by config, by SIGHUP
// and by PUT /debug/log-level — all pointing at the same variable.
type Handle struct {
	*slog.Logger

	lvl *slog.LevelVar
}

// New builds a logger from Options.
//
// It never returns a nil Handle. A bad level or format is reported, but the
// returned logger still works, so a misconfigured process can say why it is
// unhappy instead of dying silently.
func New(o Options) (*Handle, error) {
	o = o.withDefaults()

	lvl, lvlErr := resolveLevel(o.Level)
	backend, fmtErr := newBackend(o, lvl)

	log := slog.New(decorate(backend, o))
	return &Handle{Logger: withBaseAttrs(log, o), lvl: lvl}, errors.Join(lvlErr, fmtErr)
}

// withDefaults fills in the fields that have one, so nothing below has to keep
// asking whether a value was supplied.
func (o Options) withDefaults() Options {
	if o.Writer == nil {
		o.Writer = os.Stderr
	}
	if o.TimeFormat == "" {
		o.TimeFormat = time.TimeOnly
	}
	return o
}

// resolveLevel turns the configured tier name into the shared level knob.
//
// The knob is a single *slog.LevelVar that every derived logger reads, which is
// what lets the level move at runtime from a flag, a SIGHUP or an admin
// endpoint without rebuilding anything.
func resolveLevel(name string) (*slog.LevelVar, error) {
	lvl := new(slog.LevelVar)
	lvl.Set(slog.LevelInfo)

	if name == "" {
		return lvl, nil
	}
	parsed, err := ParseLevel(name)
	if err != nil {
		return lvl, err
	}
	lvl.Set(parsed)
	return lvl, nil
}

// newBackend builds the handler that actually formats records. On an unknown
// format it falls back to JSON and reports why, rather than refusing to log.
func newBackend(o Options, lvl slog.Leveler) (slog.Handler, error) {
	format, err := resolveFormat(o.Format, o.Writer)
	if err != nil {
		format = FormatJSON
	}

	if format == FormatText {
		return tint.NewTextHandler(o.Writer, &tint.Options{
			Level:       lvl,
			AddSource:   o.AddSource,
			TimeFormat:  o.TimeFormat,
			NoColor:     !colorEnabled(o.Writer),
			ReplaceAttr: replaceAttr,
		}), err
	}

	return slog.NewJSONHandler(o.Writer, &slog.HandlerOptions{
		Level:       lvl,
		AddSource:   o.AddSource,
		ReplaceAttr: replaceAttr,
	}), err
}

// decorate wraps the backend with the behaviour that must hold regardless of
// which backend was selected.
//
// Redaction is a wrapper rather than a ReplaceAttr hook precisely so it cannot
// be lost by choosing a handler that ignores ReplaceAttr.
func decorate(h slog.Handler, o Options) slog.Handler {
	h = NewRedactHandler(h)
	if !o.NoThrottle {
		h = NewThrottleHandler(h, throttleWindow)
	}
	return h
}

// withBaseAttrs attaches the identity every record carries.
func withBaseAttrs(log *slog.Logger, o Options) *slog.Logger {
	if o.Service != "" {
		log = log.With(slog.String("service", o.Service))
	}
	if o.Version != "" {
		log = log.With(slog.String("version", o.Version))
	}
	return log
}

// SetLevel moves the level of this logger and every logger derived from it.
func (h *Handle) SetLevel(name string) error {
	l, err := ParseLevel(name)
	if err != nil {
		return err
	}
	h.lvl.Set(l)
	return nil
}

// Level reports the current threshold.
func (h *Handle) Level() slog.Level { return h.lvl.Level() }

// LevelString reports the current threshold by name.
func (h *Handle) LevelString() string { return LevelName(h.lvl.Level()) }

// LevelVar exposes the shared level knob for handlers that need to read or
// write it directly (the admin listener's PUT /debug/log-level).
func (h *Handle) LevelVar() *slog.LevelVar { return h.lvl }

// ParseLevel maps a tier name to its slog level. It is case-insensitive and
// accepts the aliases slog itself understands.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trace":
		return LevelTrace, nil
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error", "err":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unknown log level %q (want one of: error, warn, info, debug, trace)", s)
	}
}

// LevelName is the inverse of ParseLevel. Levels between the named tiers render
// the way slog renders them, e.g. "INFO+2".
func LevelName(l slog.Level) string {
	switch l {
	case LevelTrace:
		return "TRACE"
	default:
		return l.String()
	}
}

func resolveFormat(format string, w io.Writer) (string, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case FormatJSON:
		return FormatJSON, nil
	case FormatText, "console":
		return FormatText, nil
	case FormatAuto, "":
		if isTerminal(w) {
			return FormatText, nil
		}
		return FormatJSON, nil
	default:
		return "", fmt.Errorf("unknown log format %q (want one of: %s, %s, %s)",
			format, FormatAuto, FormatText, FormatJSON)
	}
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// colorEnabled honours the NO_COLOR convention (https://no-color.org) and the
// FORCE_COLOR escape hatch used by CI systems that do have a colour-capable log
// viewer but no TTY.
func colorEnabled(w io.Writer) bool {
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	if v := os.Getenv("FORCE_COLOR"); v != "" && v != "0" {
		return true
	}
	return isTerminal(w)
}

// replaceAttr renders our custom TRACE level by name instead of as "DEBUG-4",
// and durations as "1.5s" instead of as a bare nanosecond count. Both are
// about the log being read by a person at 3am.
//
// Where a duration is a measurement rather than a setting, the call site uses
// an explicit dur_ms number instead, so it can be aggregated.
func replaceAttr(_ []string, a slog.Attr) slog.Attr {
	switch a.Key {
	case slog.LevelKey:
		if l, ok := a.Value.Any().(slog.Level); ok {
			a.Value = slog.StringValue(LevelName(l))
		}
		return a
	}
	if a.Value.Kind() == slog.KindDuration {
		a.Value = slog.StringValue(a.Value.Duration().String())
	}
	return a
}
