package config

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration is a time.Duration that round-trips as a human string ("30s", "5m")
// through YAML, JSON and text. The standard library's Duration marshals as an
// integer count of nanoseconds, which makes both a config file and the output
// of --print-config unreadable.
type Duration time.Duration

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// String renders the duration the way a human writes it.
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalText parses "30s", "1h30m" and friends.
func (d *Duration) UnmarshalText(b []byte) error {
	parsed, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", string(b), err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalText renders for text-based encoders.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// MarshalYAML keeps --print-config output readable.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// UnmarshalYAML accepts both a string ("30s") and a bare integer (nanoseconds),
// so a config written by a machine still loads.
func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err == nil {
		return d.UnmarshalText([]byte(s))
	}
	var n int64
	if err := unmarshal(&n); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\" or an integer of nanoseconds")
	}
	*d = Duration(n)
	return nil
}

// MarshalJSON renders for GET /debug/config.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// UnmarshalJSON accepts a string or a number.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		return d.UnmarshalText([]byte(s))
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\" or an integer of nanoseconds")
	}
	*d = Duration(n)
	return nil
}
