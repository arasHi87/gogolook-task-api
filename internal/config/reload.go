package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

// hotPrefixes are the configuration paths a SIGHUP may change on a running
// process. Anything not covered here is load-time only: changing it needs a
// restart, and a reload that touches it logs a loud warning and skips it
// rather than pretending to have applied it.
//
// The test for this list is that every entry is a real configuration path.
var hotPrefixes = []string{
	"logging.level",
	"queue.workers",
	"queue.claim_batch",
	"queue.poll_interval",
	"queue.reaper_interval",
	"queue.fetch_cooldown",
	"queue.job_timeout",
	"queue.max_attempts",
	"queue.backoff",
	"queue.retention",
	"ratelimit",
	"breaker",
	"webhook.timeout",
	"webhook.retry",
	"auth.mode",
	"auth.clients",
}

// Change is one field that differs between two configurations.
type Change struct {
	Field string
	From  string
	To    string
	// Hot reports whether this field may be changed without a restart.
	Hot bool
}

// String renders a change the way it is logged: field=X from=A to=B.
func (c Change) String() string {
	return fmt.Sprintf("%s from=%s to=%s", c.Field, c.From, c.To)
}

// Diff compares two configurations field by field and splits the result into
// what a SIGHUP can apply and what it cannot.
//
// It works off the flattened koanf representation rather than reflection over
// the struct, so a new field is covered the moment it is added — there is no
// second list of "fields to compare" to forget to update.
func Diff(old, next *Config) (hot, restartOnly []Change, err error) {
	oldMap, err := flatten(old)
	if err != nil {
		return nil, nil, err
	}
	nextMap, err := flatten(next)
	if err != nil {
		return nil, nil, err
	}

	fields := make(map[string]struct{}, len(oldMap)+len(nextMap))
	for k := range oldMap {
		fields[k] = struct{}{}
	}
	for k := range nextMap {
		fields[k] = struct{}{}
	}

	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		from, to := render(oldMap[k]), render(nextMap[k])
		if from == to {
			continue
		}
		c := Change{Field: k, From: from, To: to, Hot: IsHot(k)}
		if c.Hot {
			hot = append(hot, c)
		} else {
			restartOnly = append(restartOnly, c)
		}
	}
	return hot, restartOnly, nil
}

// IsHot reports whether a configuration path can be changed by SIGHUP.
func IsHot(path string) bool {
	for _, p := range hotPrefixes {
		if path == p || strings.HasPrefix(path, p+".") {
			return true
		}
	}
	return false
}

// ApplyHot returns a copy of old with only the hot fields taken from next.
//
// It is driven entirely by hotPrefixes: the merge happens on the flattened
// representation, so there is no second hand-maintained list of "restart-only
// groups" to fall out of sync with the first. That matters because the failure
// mode is silent — a field that is hot in one list and not the other either
// gets applied without a log line or gets ignored despite one.
//
// Keeping the load-time fields at their old values is deliberate: the live
// config's http.addr must always name the socket that is actually open.
func ApplyHot(old, next *Config) (*Config, error) {
	oldMap, err := flatten(old)
	if err != nil {
		return nil, err
	}
	nextMap, err := flatten(next)
	if err != nil {
		return nil, err
	}

	merged := make(map[string]any, len(oldMap))
	for k, v := range oldMap {
		merged[k] = v
	}
	for k, v := range nextMap {
		if IsHot(k) {
			merged[k] = v
		}
	}

	k := koanf.New(delim)
	if err := k.Load(confmap.Provider(merged, delim), nil); err != nil {
		return nil, fmt.Errorf("merge hot fields: %w", err)
	}
	out := Defaults()
	if err := unmarshal(k, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func flatten(c *Config) (map[string]any, error) {
	k := koanf.New(delim)
	if err := k.Load(structs.Provider(*c, "koanf"), nil); err != nil {
		return nil, fmt.Errorf("flatten config: %w", err)
	}
	return k.All(), nil
}

// render turns a value into its comparison and log form. Durations render as
// "30s" rather than as a nanosecond count, so a log line about a changed field
// is readable.
func render(v any) string {
	switch t := v.(type) {
	case nil:
		return "<unset>"
	case Duration:
		return t.String()
	case string:
		return t
	default:
		return fmt.Sprintf("%v", v)
	}
}
