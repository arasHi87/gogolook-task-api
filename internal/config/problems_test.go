package config_test

import (
	"strings"
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

// Nothing wrong means nothing returned: an empty Problems must not produce a
// non-nil error, or every valid config would look invalid.
func TestProblemsEmptyIsNil(t *testing.T) {
	t.Parallel()
	var p config.Problems

	if p.Len() != 0 {
		t.Errorf("Len() = %d, want 0", p.Len())
	}
	if err := p.Err(); err != nil {
		t.Errorf("Err() = %v, want nil", err)
	}
}

// Problems accumulate rather than short-circuit; that is the whole point.
func TestProblemsAccumulate(t *testing.T) {
	t.Parallel()
	var p config.Problems

	p.Add("a.one", "must be %d", 1)
	p.NotEmpty("a.two", "  ")
	p.Positive("a.three", 0)
	p.AtLeast("a.four", 0, 2)
	p.OneOf("a.five", "nope", "yes", "no")

	if p.Len() != 5 {
		t.Fatalf("Len() = %d, want 5", p.Len())
	}

	msg := p.Err().Error()
	for _, want := range []string{"a.one: must be 1", "a.two", "a.three", "a.four", "a.five"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error is missing %q:\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, "invalid configuration") {
		t.Errorf("error has no heading:\n%s", msg)
	}
}

func TestProblemsHelpersAcceptValidValues(t *testing.T) {
	t.Parallel()
	var p config.Problems

	p.NotEmpty("a", "value")
	p.Positive("b", 1)
	p.AtLeast("c", 2, 2)
	p.OneOf("d", "yes", "yes", "no")

	if p.Len() != 0 {
		t.Errorf("Len() = %d, want 0; helpers flagged a valid value: %v", p.Len(), p.Err())
	}
}

// The message has to say what was allowed, or the operator has to read source
// to fix a typo.
func TestOneOfListsTheAllowedValues(t *testing.T) {
	t.Parallel()
	var p config.Problems
	p.OneOf("storage.backend", "sqlite", "memory", "postgres")

	msg := p.Err().Error()
	for _, want := range []string{"sqlite", "memory", "postgres"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message is missing %q:\n%s", want, msg)
		}
	}
}
