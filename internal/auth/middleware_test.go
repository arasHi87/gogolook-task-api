package auth_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/auth"
	"github.com/arasHi87/gogolook-task-api/internal/config"
)

// serve runs the middleware over a handler that reports the identity it saw.
func serve(t *testing.T, mode, token string) (*http.Response, auth.Identity) {
	t.Helper()

	var seen auth.Identity
	h := auth.Middleware(auth.NewResolver(testAuth(mode), 0))(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = auth.From(r.Context())
			w.WriteHeader(http.StatusOK)
		}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(t, token))
	return rec.Result(), seen
}

func TestMiddlewarePutsTheIdentityInTheContext(t *testing.T) {
	t.Parallel()

	resp, id := serve(t, config.AuthOptional, standardToken)
	_ = resp.Body.Close()

	if id.ClientID != "acme" || id.Tier != config.TierStandard {
		t.Errorf("identity = %+v, want acme at the standard tier", id)
	}
}

// A 401 without WWW-Authenticate tells the client it needs credentials but not
// what kind, which is how a working client and a broken one look the same.
func TestRequiredModeChallenges(t *testing.T) {
	t.Parallel()

	resp, _ := serve(t, config.AuthRequired, "")
	defer resp.Body.Close() //nolint:errcheck // test response

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	if !strings.HasPrefix(challenge, "Bearer ") || !strings.Contains(challenge, `realm="taskapi"`) {
		t.Errorf("WWW-Authenticate = %q, want a Bearer challenge naming the realm", challenge)
	}
}

// The reviewer's first curl must work. This is the case that decides whether
// the default is defensible.
func TestOptionalModeNeverRejects(t *testing.T) {
	t.Parallel()

	for _, token := range []string{"", "not-a-token", standardToken} {
		resp, id := serve(t, config.AuthOptional, token)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("token %q: status %d, want 200", token, resp.StatusCode)
		}
		if id.Tier == "" {
			t.Errorf("token %q: no tier was assigned", token)
		}
	}
}
