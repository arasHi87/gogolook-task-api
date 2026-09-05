package config_test

import (
	"strings"
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

// The configuration a bare `go run` uses must itself be valid.
func TestDefaultsAreValid(t *testing.T) {
	t.Parallel()

	c := config.Defaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("the built-in defaults must be valid, got: %v", err)
	}
}

// Validation reports everything wrong at once, across sections. A config file
// with four mistakes should be fixable in one edit, not four.
func TestValidateReportsEveryProblem(t *testing.T) {
	t.Parallel()

	c := config.Defaults()
	c.HTTP.Addr = ""
	c.Logging.Level = "loud"
	c.Queue.Workers = 0
	c.Auth.Mode = "maybe"

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted a config with four problems")
	}
	for _, want := range []string{"http.addr", "logging.level", "queue.workers", "auth.mode"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %s:\n%v", want, err)
		}
	}
}

// Rules that relate two sections cannot live in either one, because neither
// can see the other. They live in Config.validateAcrossSections.
func TestCrossSectionRules(t *testing.T) {
	t.Parallel()

	t.Run("admin must not share the public port", func(t *testing.T) {
		t.Parallel()
		c := config.Defaults()
		c.Admin.Addr = c.HTTP.Addr

		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "admin.addr") {
			t.Fatalf("Validate = %v, want a problem naming admin.addr", err)
		}
		// Neither section alone can catch it.
		wantNoProblem(t, check(&c.Admin))
		wantNoProblem(t, check(&c.HTTP))
	})

	t.Run("two ephemeral listeners are not a collision", func(t *testing.T) {
		t.Parallel()
		// Port 0 means "ask the kernel", so two listeners on it get different
		// ports and the rule above has nothing to prevent. The end-to-end
		// suite starts every listener this way.
		c := config.Defaults()
		c.HTTP.Addr = "127.0.0.1:0"
		c.Admin.Addr = "127.0.0.1:0"

		if err := c.Validate(); err != nil {
			t.Fatalf("Validate = %v, want no problem", err)
		}
	})

	t.Run("a fixed port shared by both listeners is still a collision", func(t *testing.T) {
		t.Parallel()
		c := config.Defaults()
		c.HTTP.Addr = "127.0.0.1:8080"
		c.Admin.Addr = "127.0.0.1:8080"

		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "admin.addr") {
			t.Fatalf("Validate = %v, want a problem naming admin.addr", err)
		}
	})

	t.Run("a delivery attempt must fit inside the open window", func(t *testing.T) {
		t.Parallel()
		// A breaker whose open window is shorter than one attempt never gets to
		// fail fast: the probe outlives the window it was opened for.
		c := config.Defaults()
		c.Webhook.Timeout = c.Breaker.Webhook.OpenDuration + 1

		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "webhook.timeout") {
			t.Fatalf("Validate = %v, want a problem naming webhook.timeout", err)
		}
		wantNoProblem(t, check(&c.Webhook))
		wantNoProblem(t, check(&c.Breaker))
	})
}
