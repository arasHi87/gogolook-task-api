package config

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Every field of Config must appear in sections(), and its Path must match its
// koanf tag. A section that is added but not listed silently gets no defaults
// and is never validated — a failure with no symptom until production.
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

// Every problem a section reports must be addressed by its own path, so an
// operator can find the offending line without searching. This is what lets
// each section be tested in isolation in its own file.
func TestSectionProblemsArePrefixedWithTheirPath(t *testing.T) {
	t.Parallel()

	// A zero section is invalid in every case, which makes this a cheap way to
	// see everything each one can report.
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

// Every source file has a test file beside it. The point of splitting the
// package one section per file is that you can open a section and see its
// rules and its tests together; a source file with no test file breaks that
// and is easy to add without noticing.
func TestEverySourceFileHasATestFile(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	tests := map[string]struct{}{}
	var sources []string
	for _, e := range entries {
		name := e.Name()
		switch {
		case !strings.HasSuffix(name, ".go"):
		case strings.HasSuffix(name, "_test.go"):
			tests[strings.TrimSuffix(name, "_test.go")] = struct{}{}
			// An internal test file covers the same source as its external twin.
			tests[strings.TrimSuffix(name, "_internal_test.go")] = struct{}{}
		default:
			sources = append(sources, strings.TrimSuffix(name, ".go"))
		}
	}

	// helpers_test.go is shared scaffolding with no source file of its own.
	delete(tests, "helpers")

	sort.Strings(sources)
	for _, s := range sources {
		if _, ok := tests[s]; !ok {
			t.Errorf("%s.go has no %s_test.go", s, s)
		}
	}
}
