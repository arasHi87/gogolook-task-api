package config

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/posflag"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
	"github.com/spf13/pflag"
)

// EnvPrefix is the environment namespace. TASKAPI_HTTP_ADDR sets http.addr.
const EnvPrefix = "TASKAPI_"

// delim is the koanf path separator, and also the separator inside flag names:
// the flag --http.addr and the path http.addr are the same string by design.
const delim = "."

// DefaultConfigFile is looked for in the working directory when --config is not
// given. Its absence is not an error.
const DefaultConfigFile = "config.yaml"

// Options controls a Load.
type Options struct {
	// File is the --config path. Empty means: use DefaultConfigFile if it
	// exists, otherwise skip the file layer entirely.
	File string
	// Flags is the parsed flag set. Nil skips the flag layer, which is what
	// tests and the admin reload path do.
	Flags *pflag.FlagSet
	// Environ overrides the environment source. Nil means os.Environ.
	Environ func() []string
	// Overrides is applied above every other layer. Tests use it; nothing else
	// should.
	Overrides map[string]any
}

// Result is what Load produces: the merged config plus the provenance a
// operator needs to understand why it looks the way it does.
type Result struct {
	Config *Config
	// File is the config file actually read, or "" if none was.
	File string
	// Warnings are non-fatal problems: environment variables under our prefix
	// that match no known key, most often a typo. They are reported rather than
	// ignored, because a silently-ignored TASKAPI_HTTP_ADDRR is an outage.
	Warnings []string
}

// Load merges the four layers in precedence order, lowest first:
//
//  1. built-in defaults
//  2. config.yaml
//  3. TASKAPI_* environment
//  4. --flags (only those the user actually set)
//
// Step 4 is where the classic bug lives: merging every flag, set or not, lets
// pflag's zero defaults silently overwrite the file and the environment,
// inverting the precedence. posflag.Provider avoids it when it is handed the
// koanf instance, which is why one is passed here.
func Load(o Options) (*Result, error) {
	k := koanf.New(delim)
	res := &Result{}

	// 1. defaults
	if err := k.Load(structs.Provider(Defaults(), "koanf"), nil); err != nil {
		return nil, fmt.Errorf("load defaults: %w", err)
	}
	valid := keySet(k.Keys())

	// 2. config.yaml
	path, err := resolveFile(o.File)
	if err != nil {
		return nil, err
	}
	if path != "" {
		fileK := koanf.New(delim)
		if err := fileK.Load(file.Provider(path), yaml.Parser()); err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
		if err := checkUnknownKeys(path, fileK.Keys(), valid); err != nil {
			return nil, err
		}
		if err := k.Merge(fileK); err != nil {
			return nil, fmt.Errorf("merge config %s: %w", path, err)
		}
		res.File = path
	}

	// 3. environment
	unknownEnv := map[string]struct{}{}
	envProvider := env.Provider(delim, env.Opt{
		Prefix:        EnvPrefix,
		EnvironFunc:   o.Environ,
		TransformFunc: envTransform(valid, unknownEnv),
	})
	if err := k.Load(envProvider, nil); err != nil {
		return nil, fmt.Errorf("load environment: %w", err)
	}
	for name := range unknownEnv {
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("environment variable %s matches no configuration key and was ignored", name))
	}
	sort.Strings(res.Warnings)

	// 4. flags
	if o.Flags != nil {
		p := posflag.ProviderWithValue(o.Flags, delim, k, flagTransform(valid))
		if err := k.Load(p, nil); err != nil {
			return nil, fmt.Errorf("load flags: %w", err)
		}
		overrides, err := flagOverrides(o.Flags, valid)
		if err != nil {
			return nil, err
		}
		if len(overrides) > 0 {
			if err := k.Load(confmap.Provider(overrides, delim), nil); err != nil {
				return nil, fmt.Errorf("apply flag overrides: %w", err)
			}
		}
	}

	// 5. test overrides
	if len(o.Overrides) > 0 {
		if err := k.Load(confmap.Provider(o.Overrides, delim), nil); err != nil {
			return nil, fmt.Errorf("apply overrides: %w", err)
		}
	}

	cfg := Defaults()
	if err := unmarshal(k, &cfg); err != nil {
		return nil, err
	}
	res.Config = &cfg
	return res, nil
}

// unmarshal decodes the merged map into cfg. WeaklyTypedInput is on because
// environment variables are always strings: TASKAPI_QUEUE_WORKERS=16 must
// become an int, and TASKAPI_ADMIN_PPROF=false a bool.
func unmarshal(k *koanf.Koanf, cfg *Config) error {
	err := k.UnmarshalWithConf("", cfg, koanf.UnmarshalConf{
		Tag: "koanf",
		DecoderConfig: &mapstructure.DecoderConfig{
			Result:           cfg,
			TagName:          "koanf",
			WeaklyTypedInput: true,
			Metadata:         nil,
			DecodeHook: mapstructure.ComposeDecodeHookFunc(
				mapstructure.TextUnmarshallerHookFunc(),
				mapstructure.StringToTimeDurationHookFunc(),
				mapstructure.StringToSliceHookFunc(","),
			),
		},
	})
	if err != nil {
		return fmt.Errorf("decode configuration: %w", err)
	}
	return nil
}

// resolveFile returns the config path to read, or "" for none. An explicit
// --config that does not exist is an error; a missing default file is not.
func resolveFile(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("config file %s: %w", explicit, err)
		}
		return explicit, nil
	}
	if _, err := os.Stat(DefaultConfigFile); err == nil {
		return DefaultConfigFile, nil
	}
	return "", nil
}

// keySet indexes the valid configuration paths twice: by path (http.addr) and
// by their environment spelling (http_addr). Deriving both from the struct is
// what makes the three spellings provably the same knob — there is no second
// list to keep in sync.
type keyIndex struct {
	paths map[string]struct{}
	byEnv map[string]string
}

func keySet(keys []string) keyIndex {
	idx := keyIndex{
		paths: make(map[string]struct{}, len(keys)),
		byEnv: make(map[string]string, len(keys)),
	}
	for _, k := range keys {
		idx.paths[k] = struct{}{}
		idx.byEnv[strings.ReplaceAll(k, delim, "_")] = k
	}
	return idx
}

// EnvName is the environment variable that sets a given configuration path.
// It is the inverse of the env transform and exists so documentation and error
// messages cannot drift from the loader.
func EnvName(path string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(path, delim, "_"))
}

// envTransform maps TASKAPI_HTTP_READ_HEADER_TIMEOUT to http.read_header_timeout.
//
// A naive "_ becomes ." rule cannot do this: it would produce
// http.read.header.timeout. The mapping is instead resolved against the set of
// real configuration paths, so multi-word keys work and typos are detected
// rather than silently creating a key nobody reads.
func envTransform(valid keyIndex, unknown map[string]struct{}) func(string, string) (string, any) {
	return func(name, value string) (string, any) {
		key := strings.ToLower(strings.TrimPrefix(name, EnvPrefix))
		path, ok := valid.byEnv[key]
		if !ok {
			unknown[name] = struct{}{}
			return "", nil
		}
		return path, value
	}
}

// flagTransform drops the flags that are not configuration paths (--config,
// --print-config, -v, --trace, --set). They are handled explicitly.
func flagTransform(valid keyIndex) func(string, string) (string, any) {
	return func(key, value string) (string, any) {
		if _, ok := valid.paths[key]; !ok {
			return "", nil
		}
		return key, value
	}
}

func checkUnknownKeys(path string, keys []string, valid keyIndex) error {
	var bad []string
	for _, k := range keys {
		if _, ok := valid.paths[k]; !ok {
			bad = append(bad, k)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("%s: unknown configuration key(s): %s", path, strings.Join(bad, ", "))
}
