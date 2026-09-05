package config

import (
	"fmt"
	"strings"
)

// Log levels, in the order they widen. TRACE is this service's fourth tier and
// is not part of slog's own vocabulary; internal/logging maps it.
const (
	LevelError = "error"
	LevelWarn  = "warn"
	LevelInfo  = "info"
	LevelDebug = "debug"
	LevelTrace = "trace"
)

// Log output formats. auto picks text on a terminal and JSON otherwise.
const (
	FormatAuto = "auto"
	FormatText = "text"
	FormatJSON = "json"
)

// Logging maps to the three tiers in internal/logging.
type Logging struct {
	Level  string `koanf:"level"  yaml:"level"  json:"level"`
	Format string `koanf:"format" yaml:"format" json:"format"`
}

// Path implements Section.
func (Logging) Path() string { return "logging" }

// SetDefaults implements Section.
func (l *Logging) SetDefaults() {
	*l = Logging{
		Level:  LevelInfo,
		Format: FormatAuto,
	}
}

// Validate implements Section.
func (l *Logging) Validate(p *Problems) {
	if err := checkLevel(l.Level); err != nil {
		p.Add(join(l.Path(), "level"), "%s", err)
	}
	p.OneOf(join(l.Path(), "format"), l.Format, FormatAuto, FormatText, FormatJSON)
}

// checkLevel names the level vocabulary here rather than importing
// internal/logging, so config stays a leaf package with no internal imports.
// A test in internal/logging asserts the two lists agree.
func checkLevel(s string) error {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case LevelTrace, LevelDebug, LevelInfo, LevelWarn, "warning", LevelError, "err":
		return nil
	default:
		return fmt.Errorf("%q is not one of: %s, %s, %s, %s, %s",
			s, LevelError, LevelWarn, LevelInfo, LevelDebug, LevelTrace)
	}
}
