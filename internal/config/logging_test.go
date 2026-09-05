package config_test

import (
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

func TestLoggingDefaults(t *testing.T) {
	t.Parallel()
	l := config.Defaults().Logging

	if l.Level != config.LevelInfo {
		t.Errorf("level = %q, want %q: the default tier is the lifecycle narrative only", l.Level, config.LevelInfo)
	}
	if l.Format != config.FormatAuto {
		t.Errorf("format = %q, want %q", l.Format, config.FormatAuto)
	}
	wantNoProblem(t, check(&l))
}

func TestLoggingAcceptsEveryTier(t *testing.T) {
	t.Parallel()
	// The vocabulary is duplicated in internal/logging so config stays a leaf
	// package; a test there asserts the two lists agree.
	for _, level := range []string{
		config.LevelError, config.LevelWarn, config.LevelInfo,
		config.LevelDebug, config.LevelTrace, "warning", "err", "TRACE",
	} {
		l := config.Defaults().Logging
		l.Level = level
		if err := check(&l); err != nil {
			t.Errorf("level %q was rejected: %v", level, err)
		}
	}
}

func TestLoggingValidate(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		mutate func(*config.Logging)
		field  string
	}{
		"unknown level":  {func(l *config.Logging) { l.Level = "loud" }, "logging.level"},
		"unknown format": {func(l *config.Logging) { l.Format = "interpretive dance" }, "logging.format"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l := config.Defaults().Logging
			tc.mutate(&l)
			wantProblem(t, check(&l), tc.field)
		})
	}

	t.Run("every format is accepted", func(t *testing.T) {
		t.Parallel()
		for _, f := range []string{config.FormatAuto, config.FormatText, config.FormatJSON} {
			l := config.Defaults().Logging
			l.Format = f
			wantNoProblem(t, check(&l))
		}
	})
}
