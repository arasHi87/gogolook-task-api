package config_test

import (
	"strings"
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

func hex64(c byte) string { return strings.Repeat(string(c), 64) }

// optional is the default and it never returns 401: a reviewer's first curl
// must work. Auth here is a quota dimension, not an authorisation system.
func TestAuthDefaultsToOptional(t *testing.T) {
	t.Parallel()
	a := config.Defaults().Auth

	if a.Mode != config.AuthOptional {
		t.Errorf("mode = %q, want optional", a.Mode)
	}
	if a.Realm == "" {
		t.Error("realm is empty; a 401 needs one for WWW-Authenticate")
	}
	if len(a.Clients) == 0 {
		t.Fatal("no demo clients, so the rate-limit demo cannot work out of the box")
	}
	for _, c := range a.Clients {
		if len(c.TokenSHA256) != 64 {
			t.Errorf("client %q stores %q, want a 64-character SHA-256", c.ID, c.TokenSHA256)
		}
	}
	wantNoProblem(t, check(&a))
}

func TestAuthValidate(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		mutate func(*config.Auth)
		field  string
	}{
		"unknown mode":  {func(a *config.Auth) { a.Mode = "maybe" }, "auth.mode"},
		"empty realm":   {func(a *config.Auth) { a.Realm = "" }, "auth.realm"},
		"empty id":      {func(a *config.Auth) { a.Clients[0].ID = "" }, "auth.clients[0].id"},
		"unknown tier":  {func(a *config.Auth) { a.Clients[0].Tier = "platinum" }, "auth.clients[0].tier"},
		"empty token":   {func(a *config.Auth) { a.Clients[0].TokenSHA256 = "" }, "auth.clients[0].token_sha256"},
		"short token":   {func(a *config.Auth) { a.Clients[0].TokenSHA256 = "abc123" }, "token_sha256"},
		"non-hex token": {func(a *config.Auth) { a.Clients[0].TokenSHA256 = strings.Repeat("z", 64) }, "token_sha256"},
		// The mistake this exists to catch: pasting the token itself into the
		// field that is supposed to hold its hash.
		"plaintext token": {
			func(a *config.Auth) { a.Clients[0].TokenSHA256 = "demo-standard-token" },
			"token_sha256",
		},
		"duplicate id": {
			func(a *config.Auth) { a.Clients[1].ID = a.Clients[0].ID },
			"duplicate client id",
		},
		"duplicate token": {
			func(a *config.Auth) { a.Clients[1].TokenSHA256 = a.Clients[0].TokenSHA256 },
			"duplicate token",
		},
		// Every request would be rejected, which is never what was meant.
		"required with no clients": {
			func(a *config.Auth) { a.Mode = config.AuthRequired; a.Clients = nil },
			"auth.clients",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := config.Defaults().Auth
			tc.mutate(&a)
			wantProblem(t, check(&a), tc.field)
		})
	}
}

func TestAuthAcceptsEveryMode(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{config.AuthOptional, config.AuthRequired, config.AuthOff} {
		a := config.Defaults().Auth
		a.Mode = mode
		wantNoProblem(t, check(&a))
	}
}

// A duplicate token means two clients are indistinguishable, so the metrics
// attribute traffic to whichever one the lookup happened to find first.
func TestAuthTokenComparisonIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	a := config.Defaults().Auth
	a.Clients[0].TokenSHA256 = strings.ToLower(hex64('a'))
	a.Clients[1].TokenSHA256 = strings.ToUpper(hex64('a'))

	wantProblem(t, check(&a), "duplicate token")
}

func TestAuthEmptyClientListIsFineWhenOptional(t *testing.T) {
	t.Parallel()
	a := config.Defaults().Auth
	a.Mode = config.AuthOptional
	a.Clients = nil

	wantNoProblem(t, check(&a))
}
