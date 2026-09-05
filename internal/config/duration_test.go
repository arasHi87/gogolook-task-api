package config_test

import (
	"encoding/json"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

// The reason this type exists: time.Duration marshals as a nanosecond count,
// which makes both a config file and --print-config unreadable.
func TestDurationRendersAsAHumanWritesIt(t *testing.T) {
	t.Parallel()
	cases := map[time.Duration]string{
		30 * time.Second:       "30s",
		5 * time.Minute:        "5m0s",
		200 * time.Millisecond: "200ms",
		7 * 24 * time.Hour:     "168h0m0s",
	}
	for in, want := range cases {
		d := config.Duration(in)
		if got := d.String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
		if got := d.D(); got != in {
			t.Errorf("D() = %v, want %v", got, in)
		}
	}
}

func TestDurationRoundTrips(t *testing.T) {
	t.Parallel()
	want := config.Duration(90 * time.Second)

	t.Run("yaml", func(t *testing.T) {
		t.Parallel()
		out, err := yaml.Marshal(want)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(out) != "1m30s\n" {
			t.Errorf("yaml = %q, want \"1m30s\\n\"", out)
		}

		var got config.Duration
		if err := yaml.Unmarshal(out, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got != want {
			t.Errorf("round trip = %v, want %v", got, want)
		}
	})

	t.Run("json", func(t *testing.T) {
		t.Parallel()
		out, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(out) != `"1m30s"` {
			t.Errorf("json = %s, want \"1m30s\"", out)
		}

		var got config.Duration
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got != want {
			t.Errorf("round trip = %v, want %v", got, want)
		}
	})
}

// A config written by a machine may carry a bare nanosecond count; it should
// still load.
func TestDurationAcceptsARawNumber(t *testing.T) {
	t.Parallel()
	want := config.Duration(90 * time.Second)

	var fromYAML config.Duration
	if err := yaml.Unmarshal([]byte("90000000000"), &fromYAML); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	if fromYAML != want {
		t.Errorf("yaml number = %v, want %v", fromYAML, want)
	}

	var fromJSON config.Duration
	if err := json.Unmarshal([]byte("90000000000"), &fromJSON); err != nil {
		t.Fatalf("json: %v", err)
	}
	if fromJSON != want {
		t.Errorf("json number = %v, want %v", fromJSON, want)
	}
}

func TestDurationRejectsNonsense(t *testing.T) {
	t.Parallel()
	var d config.Duration

	for _, in := range []string{"soon", "30 seconds", ""} {
		if err := d.UnmarshalText([]byte(in)); err == nil {
			t.Errorf("UnmarshalText(%q) = nil, want an error naming the value", in)
		}
	}
	if err := yaml.Unmarshal([]byte("[1, 2]"), &d); err == nil {
		t.Error("a YAML sequence was accepted as a duration")
	}
}
