package config

import (
	"reflect"
	"strings"
	"testing"
)

// Every field of Config must appear in sections(), and its Path must match its
// koanf tag. Adding a section and forgetting to list it would mean it silently
// gets no defaults and is never validated — a failure with no symptom until
// production.
func TestEveryConfigFieldIsASection(t *testing.T) {
	t.Parallel()

	var c Config
	sections := c.sections()
	typ := reflect.TypeOf(c)

	if len(sections) != typ.NumField() {
		t.Fatalf("Config has %d fields but sections() lists %d; a section is missing from the table",
			typ.NumField(), len(sections))
	}

	for i := range typ.NumField() {
		field := typ.Field(i)
		tag := strings.Split(field.Tag.Get("koanf"), ",")[0]

		if got := sections[i].Path(); got != tag {
			t.Errorf("field %s: Path() = %q, want the koanf tag %q", field.Name, got, tag)
		}
	}
}

// sections() must hand out pointers into the receiver, or SetDefaults would
// fill in a copy and Defaults would return a zero value.
func TestSectionsPointAtTheLiveConfig(t *testing.T) {
	t.Parallel()

	var c Config
	for _, s := range c.sections() {
		s.SetDefaults()
	}
	if c.HTTP.Addr == "" || c.Queue.Workers == 0 || c.Service.Name == "" {
		t.Fatalf("SetDefaults did not reach the receiver: %+v", c)
	}
}

// The defaults a bare `go run` uses must themselves be valid.
func TestDefaultsAreValid(t *testing.T) {
	t.Parallel()

	c := Defaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("the built-in defaults must be valid, got: %v", err)
	}
}

// Every problem a section reports must be addressed by its own path, so an
// operator can find the offending line without searching.
func TestSectionProblemsArePrefixedWithTheirPath(t *testing.T) {
	t.Parallel()

	// A zero section is invalid in every case, which makes this a cheap way to
	// see what each one reports.
	var c Config
	for _, s := range c.sections() {
		var p Problems
		s.Validate(&p)

		for _, item := range p.items {
			field := strings.SplitN(item, ":", 2)[0]
			if field != s.Path() && !strings.HasPrefix(field, s.Path()+".") &&
				!strings.HasPrefix(field, s.Path()+"[") {
				t.Errorf("section %q reported a problem outside its own path: %q", s.Path(), item)
			}
		}
	}
}

// A section that is off does not have to justify settings it will not use.
func TestDisabledSectionsSkipTheirOwnRules(t *testing.T) {
	t.Parallel()

	t.Run("memory backend ignores the pool settings", func(t *testing.T) {
		c := Defaults()
		c.Storage.Backend = BackendMemory
		c.Storage.Postgres.MaxConns = 0 // nonsense, but unreachable
		if err := c.Validate(); err != nil {
			t.Errorf("Validate = %v, want nil", err)
		}
	})

	t.Run("disabled rate limiting ignores its tiers", func(t *testing.T) {
		c := Defaults()
		c.RateLimit.Enabled = false
		c.RateLimit.Tiers.Anonymous.Rate = 0
		if err := c.Validate(); err != nil {
			t.Errorf("Validate = %v, want nil", err)
		}
	})
}

// Rules that relate two sections cannot live in either one.
func TestCrossSectionRules(t *testing.T) {
	t.Parallel()

	t.Run("admin must not share the public port", func(t *testing.T) {
		c := Defaults()
		c.Admin.Addr = c.HTTP.Addr
		assertProblem(t, c.Validate(), "admin.addr")
	})

	t.Run("a delivery attempt must fit inside the open window", func(t *testing.T) {
		c := Defaults()
		c.Webhook.Timeout = c.Breaker.Webhook.OpenDuration + 1
		assertProblem(t, c.Validate(), "webhook.timeout")
	})
}

func assertProblem(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Validate = nil, want a problem mentioning %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("Validate = %v, want it to mention %q", err, want)
	}
}
