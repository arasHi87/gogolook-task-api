package config

import (
	"errors"
	"fmt"
	"strings"
)

// Problems collects configuration errors.
//
// Validation appends rather than returning, so a section reports everything
// wrong with it in one pass. A config file with four mistakes should be fixable
// in one edit, not four.
type Problems struct {
	items []string
}

// Add records a problem against a configuration path.
func (p *Problems) Add(field, format string, args ...any) {
	p.items = append(p.items, field+": "+fmt.Sprintf(format, args...))
}

// NotEmpty requires a non-blank string.
func (p *Problems) NotEmpty(field, value string) {
	if strings.TrimSpace(value) == "" {
		p.Add(field, "must not be empty")
	}
}

// Positive requires a duration greater than zero.
func (p *Problems) Positive(field string, d Duration) {
	if d <= 0 {
		p.Add(field, "must be greater than zero")
	}
}

// AtLeast requires an integer floor.
func (p *Problems) AtLeast(field string, value, min int) {
	if value < min {
		p.Add(field, "must be at least %d, got %d", min, value)
	}
}

// OneOf requires membership in a closed set.
func (p *Problems) OneOf(field, value string, allowed ...string) {
	for _, a := range allowed {
		if value == a {
			return
		}
	}
	p.Add(field, "%q is not one of: %s", value, strings.Join(allowed, ", "))
}

// Len reports how many problems have been recorded.
func (p *Problems) Len() int { return len(p.items) }

// Err returns nil when there are no problems, and otherwise one error listing
// all of them, indented under a single heading.
func (p *Problems) Err() error {
	if len(p.items) == 0 {
		return nil
	}
	return fmt.Errorf("invalid configuration:\n  %w", errors.New(strings.Join(p.items, "\n  ")))
}

// join prefixes a path segment onto a section-relative field name, so a
// section can report "queue.backoff.base" without knowing where it is mounted.
func join(prefix, field string) string {
	if prefix == "" {
		return field
	}
	return prefix + "." + field
}
