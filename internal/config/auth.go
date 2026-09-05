package config

import (
	"fmt"
	"regexp"
	"strings"
)

// Auth modes.
const (
	// AuthOptional is the default and never returns 401: a caller with no token
	// is served at the anonymous tier. A reviewer's first curl must work.
	AuthOptional = "optional"
	// AuthRequired rejects an unknown caller with 401.
	AuthRequired = "required"
	// AuthOff treats every caller as anonymous.
	AuthOff = "off"
)

// Demo credentials.
//
// The plaintext tokens are documented in .env.example and the README so the
// rate-limit demo works out of the box; only their hashes live here. They are
// development credentials by construction — a deployment that cares replaces
// the clients list.
//
// G101 flags these; it is wrong. They are SHA-256 digests, and the point of
// storing the hash rather than the token is that possessing it grants nothing.
//
//nolint:gosec // G101: hashes, not credentials
const (
	DemoStandardTokenSHA256 = "064462d4b78abaa304c5b4ab22e4a29119dbf18b3a8d7fc86e2fc633429f7dce"
	DemoInternalTokenSHA256 = "46f8c89e2172e93598e17b2a9e0261bb7848cfe389bf3d46bb98cc50c8b4710c"
)

var sha256Hex = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// Auth resolves a bearer token to a client id and a rate-limit tier.
//
// It is a quota dimension, not an authorisation system, and the README says so.
// It exists to give the limiter and the dashboards a real tenant to key on.
type Auth struct {
	Mode    string       `koanf:"mode"    yaml:"mode"    json:"mode"`
	Realm   string       `koanf:"realm"   yaml:"realm"   json:"realm"`
	Clients []AuthClient `koanf:"clients" yaml:"clients" json:"clients"`
}

// AuthClient is one configured caller. Only the hash is ever stored.
type AuthClient struct {
	ID          string `koanf:"id"           yaml:"id"           json:"id"`
	Tier        string `koanf:"tier"         yaml:"tier"         json:"tier"`
	TokenSHA256 string `koanf:"token_sha256" yaml:"token_sha256" json:"token_sha256"`
}

// Path implements Section.
func (Auth) Path() string { return "auth" }

// SetDefaults implements Section.
func (a *Auth) SetDefaults() {
	*a = Auth{
		Mode:  AuthOptional,
		Realm: "taskapi",
		Clients: []AuthClient{
			{ID: "demo", Tier: TierStandard, TokenSHA256: DemoStandardTokenSHA256},
			{ID: "worker", Tier: TierInternal, TokenSHA256: DemoInternalTokenSHA256},
		},
	}
}

// Validate implements Section.
func (a *Auth) Validate(p *Problems) {
	at := func(f string) string { return join(a.Path(), f) }

	p.OneOf(at("mode"), a.Mode, AuthOptional, AuthRequired, AuthOff)
	p.NotEmpty(at("realm"), a.Realm)

	a.validateClients(p)

	if a.Mode == AuthRequired && len(a.Clients) == 0 {
		p.Add(at("clients"),
			"auth.mode is required but no clients are configured: every request would be rejected")
	}
}

// validateClients checks the list for the two mistakes that make it useless: a
// duplicate id (so metrics attribute traffic to the wrong tenant) and a
// duplicate token (so two clients are indistinguishable).
func (a *Auth) validateClients(p *Problems) {
	seenID := map[string]int{}
	seenToken := map[string]string{}

	for i, c := range a.Clients {
		at := func(f string) string { return fmt.Sprintf("%s.clients[%d].%s", a.Path(), i, f) }

		if c.ID == "" {
			p.Add(at("id"), "must not be empty")
		} else {
			if prev, dup := seenID[c.ID]; dup {
				p.Add(at("id"), "duplicate client id %q (also at auth.clients[%d])", c.ID, prev)
			}
			seenID[c.ID] = i
		}

		p.OneOf(at("tier"), c.Tier, TierAnonymous, TierStandard, TierInternal)

		switch {
		case c.TokenSHA256 == "":
			p.Add(at("token_sha256"), "must not be empty")
		case !sha256Hex.MatchString(c.TokenSHA256):
			p.Add(at("token_sha256"),
				"must be 64 hexadecimal characters: store the SHA-256 of the token, never the token itself")
		default:
			lower := strings.ToLower(c.TokenSHA256)
			if prev, dup := seenToken[lower]; dup {
				p.Add(at("token_sha256"), "duplicate token, already used by client %q", prev)
			}
			seenToken[lower] = c.ID
		}
	}
}
