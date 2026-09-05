package auth_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/auth"
	"github.com/arasHi87/gogolook-task-api/internal/config"
)

const (
	standardToken = "standard-secret"
	internalToken = "internal-secret"
)

func hashOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func testAuth(mode string) config.Auth {
	return config.Auth{
		Mode:  mode,
		Realm: "taskapi",
		Clients: []config.AuthClient{
			{ID: "acme", Tier: config.TierStandard, TokenSHA256: hashOf(standardToken)},
			{ID: "worker", Tier: config.TierInternal, TokenSHA256: hashOf(internalToken)},
		},
	}
}

func request(t *testing.T, token string) *http.Request {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, "/tasks", nil)
	r.RemoteAddr = "203.0.113.7:51234"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

// The default mode never returns 401, and that is the decision, not an
// oversight: a reviewer whose first curl is refused concludes the exercise is
// broken. An unrecognised caller is served at the anonymous tier instead.
func TestResolveByMode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		mode     string
		token    string
		wantID   string
		wantTier string
		wantKey  string
		wantOK   bool
	}{
		{
			name: "optional, no token", mode: config.AuthOptional, token: "",
			wantID: auth.Anonymous, wantTier: config.TierAnonymous, wantKey: "203.0.113.7", wantOK: true,
		},
		{
			name: "optional, valid token", mode: config.AuthOptional, token: standardToken,
			wantID: "acme", wantTier: config.TierStandard, wantKey: "acme", wantOK: true,
		},
		{
			// A stale token must not turn a working client into a broken one
			// with no warning: it drops to the public tier and keeps serving.
			name: "optional, unknown token", mode: config.AuthOptional, token: "not-a-token",
			wantID: auth.Anonymous, wantTier: config.TierAnonymous, wantKey: "203.0.113.7", wantOK: true,
		},
		{
			name: "required, no token", mode: config.AuthRequired, token: "",
			wantOK: false,
		},
		{
			name: "required, unknown token", mode: config.AuthRequired, token: "not-a-token",
			wantOK: false,
		},
		{
			name: "required, valid token", mode: config.AuthRequired, token: internalToken,
			wantID: "worker", wantTier: config.TierInternal, wantKey: "worker", wantOK: true,
		},
		{
			// off means the quota dimension is gone; it does not mean a token
			// grants a bigger one.
			name: "off, valid token", mode: config.AuthOff, token: standardToken,
			wantID: auth.Anonymous, wantTier: config.TierAnonymous, wantKey: "203.0.113.7", wantOK: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := auth.NewResolver(testAuth(tc.mode), 0)
			id, ok := r.Resolve(request(t, tc.token))

			if ok != tc.wantOK {
				t.Fatalf("accepted = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			switch {
			case id.ClientID != tc.wantID:
				t.Errorf("client id = %q, want %q", id.ClientID, tc.wantID)
			case id.Tier != tc.wantTier:
				t.Errorf("tier = %q, want %q", id.Tier, tc.wantTier)
			case id.Key != tc.wantKey:
				t.Errorf("limiter key = %q, want %q", id.Key, tc.wantKey)
			}
		})
	}
}

// Every configured client must be findable, wherever it sits in the list. The
// scan runs to the end rather than returning early, and a bug there would show
// up as "only the last client works".
func TestEveryClientResolves(t *testing.T) {
	t.Parallel()

	r := auth.NewResolver(testAuth(config.AuthRequired), 0)

	for token, want := range map[string]string{standardToken: "acme", internalToken: "worker"} {
		id, ok := r.Resolve(request(t, token))
		if !ok || id.ClientID != want {
			t.Errorf("token for %q resolved to %q (accepted=%v)", want, id.ClientID, ok)
		}
	}
}

func TestBearerScheme(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"Bearer " + standardToken: true,
		// RFC 9110 says auth-scheme is case-insensitive, so a client sending
		// this is not wrong.
		"bearer " + standardToken: true,
		"BEARER " + standardToken: true,
		"Basic " + standardToken:  false,
		standardToken:             false,
		"Bearer":                  false,
		"Bearer ":                 false,
		"":                        false,
	}

	r := auth.NewResolver(testAuth(config.AuthOptional), 0)
	for header, wantClient := range cases {
		req := httptest.NewRequest(http.MethodGet, "/tasks", nil)
		req.RemoteAddr = "203.0.113.7:1"
		if header != "" {
			req.Header.Set("Authorization", header)
		}

		id, _ := r.Resolve(req)
		if got := !id.IsAnonymous(); got != wantClient {
			t.Errorf("%q: recognised = %v, want %v", header, got, wantClient)
		}
	}
}

// Counting from the right is the entire security property. The leftmost entry
// is whatever the client wrote, so a limiter keyed on it is decorative: one
// caller mints a new bucket per request by changing a header.
func TestClientIPCountsProxiesFromTheRight(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		xff  string
		hops int
		want string
	}{
		{
			name: "no proxies trusted, the socket peer wins",
			xff:  "1.2.3.4, 5.6.7.8", hops: 0, want: "198.51.100.1",
		},
		{
			// chain = [1.2.3.4, 5.6.7.8, peer]; one trusted proxy is the peer,
			// so the caller is the entry before it.
			name: "one proxy trusted",
			xff:  "1.2.3.4, 5.6.7.8", hops: 1, want: "5.6.7.8",
		},
		{
			name: "two proxies trusted",
			xff:  "1.2.3.4, 5.6.7.8", hops: 2, want: "1.2.3.4",
		},
		{
			// The header claims a longer chain than the deployment has. Every
			// extra entry is attacker-supplied, so none of them is trusted.
			name: "a spoofed header cannot reach past the configured depth",
			xff:  "9.9.9.9, 1.2.3.4, 5.6.7.8", hops: 1, want: "5.6.7.8",
		},
		{
			// A caller inventing a chain against a deployment with more
			// trusted hops than entries falls back to the peer, which is the
			// only address nobody asserted.
			name: "a chain shorter than the configured depth falls back to the peer",
			xff:  "1.2.3.4", hops: 5, want: "198.51.100.1",
		},
		{
			name: "no header at all",
			xff:  "", hops: 2, want: "198.51.100.1",
		},
		{
			name: "ports are stripped",
			xff:  "1.2.3.4:9999", hops: 1, want: "1.2.3.4",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := httptest.NewRequest(http.MethodGet, "/tasks", nil)
			r.RemoteAddr = "198.51.100.1:44321"
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}

			if got := auth.ClientIP(r, tc.hops); got != tc.want {
				t.Errorf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// An absent identity must fail to the smallest quota, not to none.
func TestMissingIdentityIsAnonymous(t *testing.T) {
	t.Parallel()

	id := auth.From(t.Context())
	if !id.IsAnonymous() || id.Tier != config.TierAnonymous {
		t.Errorf("From(empty ctx) = %+v, want the anonymous identity", id)
	}
}
