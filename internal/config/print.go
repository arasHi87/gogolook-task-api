package config

import (
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// RedactedValue is what a secret renders as in --print-config and in
// GET /debug/config.
const RedactedValue = "«redacted»"

// Redacted returns a copy with every secret masked. It is what --print-config
// and the admin config endpoint render, so there is exactly one definition of
// "which fields are secret" and it cannot drift between the two.
//
// Token hashes are not masked: a SHA-256 is not a credential, and showing it is
// how an operator confirms which client list is loaded.
func (c *Config) Redacted() *Config {
	if c == nil {
		return nil
	}
	out := *c
	out.Auth.Clients = append([]AuthClient(nil), c.Auth.Clients...)

	if out.Storage.Postgres.DSN != "" {
		out.Storage.Postgres.DSN = RedactedValue
	}
	return &out
}

// WriteYAML renders the effective configuration as YAML with secrets masked.
// It goes to stdout, leaving stderr for the log stream.
func (c *Config) WriteYAML(w io.Writer) error {
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(c.Redacted()); err != nil {
		return fmt.Errorf("render config: %w", err)
	}
	return enc.Close()
}

// String renders the redacted configuration as YAML, for error messages and
// tests. It never returns an error; a config that cannot be marshalled is a
// programming mistake, and the text says so.
func (c *Config) String() string {
	b, err := yaml.Marshal(c.Redacted())
	if err != nil {
		return fmt.Sprintf("<unrenderable config: %v>", err)
	}
	return string(b)
}
