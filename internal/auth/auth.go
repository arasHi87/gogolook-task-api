// Package auth resolves a caller to an identity.
//
// It is a quota dimension, not an authorisation system, and the difference is
// the point. There are no scopes, no sessions and no 403: a token buys a rate
// limit and a name in the dashboards, nothing else. Saying that plainly is
// more honest than shipping a half-built auth system that looks like a whole
// one.
//
// The default mode never returns 401. A reviewer whose first `curl -X POST
// localhost:8080/tasks` is refused concludes the exercise is broken, so an
// unrecognised caller is served at the anonymous tier instead of rejected.
// That turns auth from a gate into a quota dimension, which is what real APIs
// with public tiers actually do.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"strings"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

// Anonymous is the client id of every caller without a recognised token.
//
// A literal constant, never the caller's IP. The id is a metric label, and a
// label whose values come from the network is an unbounded, attacker-
// controlled cardinality bomb that takes the metrics backend down.
const Anonymous = "anonymous"

// Identity is who the caller is.
type Identity struct {
	// ClientID names the caller. It is bounded by construction: either an id
	// from the configured list, or the literal "anonymous".
	ClientID string
	// Tier selects the quota.
	Tier string
	// Key is what the rate limiter counts against. For a configured client it
	// is the client id, so the quota follows the token across addresses. For
	// an anonymous caller it is the IP, which is the only thing there is —
	// and it stays here, out of the metric labels.
	Key string
}

// IsAnonymous reports whether the caller presented no recognised token.
func (i Identity) IsAnonymous() bool { return i.ClientID == Anonymous }

// Resolver maps a request to an Identity.
type Resolver struct {
	mode  string
	realm string
	// clients is a slice, not a map keyed by digest, so the lookup is a
	// constant-time scan. See Resolve.
	clients []client
	hops    int
}

type client struct {
	id     string
	tier   string
	digest []byte
}

// NewResolver builds a resolver from configuration.
//
// hops comes from http.trusted_proxy_hops rather than the auth section,
// because it is a property of the deployment's network, not of its callers.
func NewResolver(cfg config.Auth, hops int) *Resolver {
	r := &Resolver{mode: cfg.Mode, realm: cfg.Realm, hops: hops}

	for _, c := range cfg.Clients {
		digest, err := hex.DecodeString(strings.ToLower(c.TokenSHA256))
		if err != nil {
			// Validation rejects a non-hex digest before this runs, so this is
			// unreachable rather than tolerated. Skipping is still the right
			// failure: a client that cannot be matched is a client that gets
			// the anonymous tier, not one that crashes the process.
			continue
		}
		r.clients = append(r.clients, client{id: c.ID, tier: c.Tier, digest: digest})
	}
	return r
}

// Realm is the value for the WWW-Authenticate challenge.
func (r *Resolver) Realm() string { return r.realm }

// Mode reports the configured behaviour for an unrecognised caller.
func (r *Resolver) Mode() string { return r.mode }

// Resolve identifies the caller, and reports whether a token was presented and
// rejected.
//
// The scan is constant time in two directions, and both matter. Comparing the
// presented digest with subtle.ConstantTimeCompare rather than == removes the
// timing oracle on the token itself; running the whole list and keeping the
// match in a variable rather than returning early removes the one on *which*
// client matched, which would otherwise leak the position of a valid token in
// the configuration.
//
// It is a linear scan over a list that has two entries. A map would be O(1) and
// would reintroduce both leaks for a saving nobody can measure.
func (r *Resolver) Resolve(req *http.Request) (Identity, bool) {
	anon := Identity{ClientID: Anonymous, Tier: config.TierAnonymous, Key: ClientIP(req, r.hops)}

	if r.mode == config.AuthOff {
		return anon, true
	}

	token, ok := bearerToken(req.Header.Get("Authorization"))
	if !ok {
		// No credential offered. In optional mode that is a caller using the
		// public tier, not a caller failing to authenticate.
		return anon, r.mode != config.AuthRequired
	}

	sum := sha256.Sum256([]byte(token))
	matched := -1
	for i, c := range r.clients {
		if subtle.ConstantTimeCompare(sum[:], c.digest) == 1 {
			matched = i
		}
	}
	if matched < 0 {
		// A token was offered and is not one of ours. Optional mode still
		// serves it, at the anonymous tier — the alternative is that a stale
		// token turns a working client into a broken one with no warning.
		return anon, r.mode != config.AuthRequired
	}

	c := r.clients[matched]
	return Identity{ClientID: c.id, Tier: c.tier, Key: c.id}, true
}

// bearerToken extracts the credential from an Authorization header.
//
// The scheme is compared case-insensitively because RFC 9110 says auth-scheme
// is case-insensitive, and a client sending "bearer" is not wrong.
func bearerToken(header string) (string, bool) {
	scheme, rest, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(rest)
	return token, token != ""
}

// ClientIP returns the caller's address, trusting exactly hops proxies.
//
// The chain, from our side, is every X-Forwarded-For entry followed by the
// socket peer: each proxy appends the address it was talking to, and the peer
// is the one nobody has appended yet. Counting from the right, the first
// untrusted entry is the caller.
//
// Counting from the right is the whole security property. The leftmost entry
// is whatever the client wrote, so a limiter keyed on it is decorative: one
// caller can mint a new bucket per request by changing a header. Trusting
// nothing by default (hops = 0, use the peer) is the only safe default,
// because it is the one value that cannot be wrong about the topology.
func ClientIP(r *http.Request, hops int) string {
	peer := host(r.RemoteAddr)
	if hops <= 0 {
		return peer
	}

	chain := forwardedFor(r.Header.Get("X-Forwarded-For"))
	chain = append(chain, peer)

	// hops proxies at the right of the chain are ours; the entry before them
	// is the caller. A chain shorter than the configured depth means the
	// request did not come through the proxies we were told about, so the peer
	// is the only address that has not been asserted by someone else.
	i := len(chain) - 1 - hops
	if i < 0 {
		return peer
	}
	return chain[i]
}

func forwardedFor(header string) []string {
	if header == "" {
		return nil
	}
	parts := strings.Split(header, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, host(v))
		}
	}
	return out
}

// host strips a port if there is one. An XFF entry is usually a bare address
// and a RemoteAddr always has a port.
func host(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// identityKey addresses the identity in a context.
type identityKey struct{}

// Into returns a context carrying the identity.
func Into(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// From returns the identity, or the anonymous one if the middleware did not
// run. Failing to the least-privileged answer is the right default for
// something that selects a quota.
func From(ctx context.Context) Identity {
	if id, ok := ctx.Value(identityKey{}).(Identity); ok {
		return id
	}
	return Identity{ClientID: Anonymous, Tier: config.TierAnonymous}
}
