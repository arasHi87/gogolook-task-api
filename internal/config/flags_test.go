package config_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

// Flag names are configuration paths verbatim, which is what lets posflag map
// them with no translation table. This asserts there is no flag naming a path
// that does not exist — the failure mode being a flag that parses, merges, and
// changes nothing.
func TestEveryDottedFlagIsARealConfigurationPath(t *testing.T) {
	t.Parallel()

	seen := 0
	newFlags(t).VisitAll(func(f *pflag.Flag) {
		if !strings.Contains(f.Name, ".") {
			return // a convenience flag; covered below
		}
		seen++

		// --set refuses an unknown key, so a bad flag name fails here. Using
		// the flag's own default as the value keeps the type correct.
		if _, err := config.Load(config.Options{
			Environ: environ(nil),
			Flags:   newFlags(t, "--set", f.Name+"="+f.DefValue),
		}); err != nil {
			t.Errorf("flag --%s does not name a real configuration path: %v", f.Name, err)
		}
	})

	if seen == 0 {
		t.Fatal("no dotted flags were registered")
	}
}

// The flags that are not configuration paths are a closed set, each handled
// explicitly. A new one added without handling would silently merge as a
// top-level key.
func TestNonPathFlagsAreTheKnownConvenienceSet(t *testing.T) {
	t.Parallel()

	var got []string
	newFlags(t).VisitAll(func(f *pflag.Flag) {
		if !strings.Contains(f.Name, ".") {
			got = append(got, f.Name)
		}
	})
	sort.Strings(got)

	want := []string{
		config.FlagConfig, config.FlagDebug, config.FlagPrintConfig,
		config.FlagSet, config.FlagTrace,
	}
	sort.Strings(want)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("convenience flags = %v, want %v", got, want)
	}
}

func TestFilePathAndPrintConfigReadTheirFlags(t *testing.T) {
	t.Parallel()

	none := newFlags(t)
	if config.FilePath(none) != "" {
		t.Errorf("FilePath with no --config = %q, want empty", config.FilePath(none))
	}
	if config.PrintConfigRequested(none) {
		t.Error("PrintConfigRequested is true with no flag")
	}

	set := newFlags(t, "--config=/tmp/x.yaml", "--print-config")
	if got := config.FilePath(set); got != "/tmp/x.yaml" {
		t.Errorf("FilePath = %q, want /tmp/x.yaml", got)
	}
	if !config.PrintConfigRequested(set) {
		t.Error("PrintConfigRequested is false with --print-config given")
	}
}

func TestSetRequiresKeyEqualsValue(t *testing.T) {
	t.Parallel()

	_, err := config.Load(config.Options{
		Environ: environ(nil),
		Flags:   newFlags(t, "--set", "queue.workers"),
	})
	if err == nil || !strings.Contains(err.Error(), "key=value") {
		t.Fatalf("err = %v, want an error explaining the --set syntax", err)
	}
}

func TestUnknownSetKeyIsFatal(t *testing.T) {
	t.Parallel()
	_, err := config.Load(config.Options{
		Environ: environ(nil),
		Flags:   newFlags(t, "--set", "queue.wokers=4"),
	})
	if err == nil || !strings.Contains(err.Error(), "queue.wokers") {
		t.Fatalf("err = %v, want an error naming the unknown --set key", err)
	}
}

// -v and --trace are shorthands, and an explicit --logging.level must win over
// them: a user who typed both meant the specific one.
func TestVerbosityShorthands(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args []string
		want string
	}{
		{[]string{}, "info"},
		{[]string{"-v"}, "debug"},
		{[]string{"--debug"}, "debug"},
		{[]string{"--trace"}, "trace"},
		{[]string{"-v", "--trace"}, "trace"},
		{[]string{"-v", "--logging.level=error"}, "error"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			got := load(t, config.Options{Environ: environ(nil), Flags: newFlags(t, tc.args...)}).Config
			if got.Logging.Level != tc.want {
				t.Errorf("logging.level = %q, want %q", got.Logging.Level, tc.want)
			}
		})
	}
}
