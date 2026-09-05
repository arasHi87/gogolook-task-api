package ratelimit_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/auth"
	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/ratelimit"
)

func testConfig() config.RateLimit {
	c := config.Defaults().RateLimit
	c.Tiers.Anonymous = config.Quota{Rate: 2, Burst: 2}
	c.Tiers.Standard = config.Quota{Rate: 10, Burst: 10}
	c.Tiers.Internal = config.Quota{Rate: 100, Burst: 100}
	return c
}

func identity(clientID, tier, key string) auth.Identity {
	return auth.Identity{ClientID: clientID, Tier: tier, Key: key}
}

// The quota is per tier, and that is the reason the limiter is worth having:
// one global number would either starve the paying client or hand the same
// capacity to anyone who can open a socket.
func TestQuotaIsPerTier(t *testing.T) {
	t.Parallel()

	cases := []struct {
		tier      string
		wantBurst int
	}{
		{config.TierAnonymous, 2},
		{config.TierStandard, 10},
		{config.TierInternal, 100},
	}

	for _, tc := range cases {
		t.Run(tc.tier, func(t *testing.T) {
			t.Parallel()

			l := ratelimit.New(testConfig(), nil)
			id := identity("c", tc.tier, "key-"+tc.tier)

			allowed := 0
			for range tc.wantBurst + 5 {
				if l.Allow(id).Allowed {
					allowed++
				}
			}
			// The burst is what a caller may spend at once. A little slack
			// upward, because the bucket refills while the loop runs.
			if allowed < tc.wantBurst || allowed > tc.wantBurst+2 {
				t.Errorf("%s allowed %d of %d, want about the burst of %d",
					tc.tier, allowed, tc.wantBurst+5, tc.wantBurst)
			}
		})
	}
}

// One caller must not be able to spend another's quota.
func TestBucketsAreIndependent(t *testing.T) {
	t.Parallel()

	l := ratelimit.New(testConfig(), nil)
	noisy := identity(auth.Anonymous, config.TierAnonymous, "203.0.113.1")
	quiet := identity(auth.Anonymous, config.TierAnonymous, "203.0.113.2")

	for range 10 {
		l.Allow(noisy)
	}
	if !l.Allow(quiet).Allowed {
		t.Error("a second caller was refused because the first exhausted its own bucket")
	}
}

// The same key seen with a different tier means a different token was used. The
// old bucket would apply the old quota until it expired.
func TestATierChangeRebuildsTheBucket(t *testing.T) {
	t.Parallel()

	l := ratelimit.New(testConfig(), nil)
	key := "shared-key"

	for range 5 {
		l.Allow(identity("c", config.TierAnonymous, key))
	}
	d := l.Allow(identity("c", config.TierInternal, key))

	if !d.Allowed {
		t.Error("the upgraded tier was refused on the previous tier's exhausted bucket")
	}
	if d.Limit != 100 {
		t.Errorf("limit = %d, want the internal tier's 100", d.Limit)
	}
}

// A plain map keyed by caller is an unbounded allocation driven by attacker
// input: every new address mints a bucket that is never freed. The eviction is
// the design, not housekeeping.
func TestIdleBucketsAreEvicted(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.KeyTTL = config.Duration(10 * time.Minute)

	l := ratelimit.New(cfg, nil)
	clock := time.Now()
	ratelimit.SetClock(l, func() time.Time { return clock })

	for i := range 100 {
		l.Allow(identity(auth.Anonymous, config.TierAnonymous, "203.0.113."+strconv.Itoa(i)))
	}
	if got := l.Len(); got != 100 {
		t.Fatalf("%d buckets, want 100", got)
	}

	// Not yet idle.
	clock = clock.Add(9 * time.Minute)
	if n := l.Sweep(); n != 0 {
		t.Errorf("swept %d buckets before the TTL elapsed", n)
	}

	clock = clock.Add(2 * time.Minute)
	if n := l.Sweep(); n != 100 {
		t.Errorf("swept %d buckets, want 100", n)
	}
	if got := l.Len(); got != 0 {
		t.Errorf("%d buckets remain after the sweep", got)
	}
}

// A caller that only learns its limit by hitting it cannot pace itself, so the
// headers go on the successes too.
func TestHeadersAreOnEveryResponse(t *testing.T) {
	t.Parallel()

	l := ratelimit.New(testConfig(), nil)
	srv := httptest.NewServer(ratelimit.Middleware(l)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })))
	defer srv.Close()

	first := get(t, srv)
	defer first.Body.Close() //nolint:errcheck // test response

	if first.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", first.StatusCode)
	}

	// Both spellings. The IETF draft collapsed to the first two; the trio is
	// what GitHub, Stripe and most SDKs actually read.
	for _, h := range []string{
		"RateLimit", "RateLimit-Policy",
		"RateLimit-Limit", "RateLimit-Remaining", "RateLimit-Reset",
	} {
		if first.Header.Get(h) == "" {
			t.Errorf("%s is missing from a successful response", h)
		}
	}
	if got := first.Header.Get("RateLimit-Policy"); got != "2;w=1" {
		t.Errorf("RateLimit-Policy = %q, want %q", got, "2;w=1")
	}
}

// Remaining must never exceed Limit. It did, before the policy described the
// bucket rather than the rate: a burst of 20 refilling at 10 per second was
// advertised as "limit=10" and then reported 18 remaining, which is a header
// set no client can act on.
func TestRemainingNeverExceedsTheLimit(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	// The shipped defaults, where burst is twice the rate.
	cfg.Tiers.Anonymous = config.Quota{Rate: 10, Burst: 20}

	l := ratelimit.New(cfg, nil)
	d := l.Allow(identity(auth.Anonymous, config.TierAnonymous, "203.0.113.9"))

	if d.Remaining > d.Limit {
		t.Errorf("remaining %d exceeds limit %d", d.Remaining, d.Limit)
	}
	if d.Limit != 20 {
		t.Errorf("limit = %d, want the burst of 20", d.Limit)
	}
	if d.Window != 2*time.Second {
		t.Errorf("window = %s, want the 2s a burst of 20 takes to refill at 10/s", d.Window)
	}
}

func TestRefusalIs429WithRetryAfter(t *testing.T) {
	t.Parallel()

	l := ratelimit.New(testConfig(), nil)
	srv := httptest.NewServer(ratelimit.Middleware(l)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })))
	defer srv.Close()

	var last *http.Response
	for range 10 {
		if last != nil {
			_ = last.Body.Close()
		}
		last = get(t, srv)
		if last.StatusCode == http.StatusTooManyRequests {
			break
		}
	}
	defer last.Body.Close() //nolint:errcheck // test response

	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 after exhausting the burst", last.StatusCode)
	}
	// Retry-After of 0 invites an immediate retry, which is the request that
	// was just refused.
	after, err := strconv.Atoi(last.Header.Get("Retry-After"))
	if err != nil || after < 1 {
		t.Errorf("Retry-After = %q, want an integer of at least 1", last.Header.Get("Retry-After"))
	}
	if got := last.Header.Get("RateLimit-Remaining"); got != "0" {
		t.Errorf("RateLimit-Remaining = %q, want 0", got)
	}
	if ct := last.Header.Get("Content-Type"); !strings.Contains(ct, "problem+json") {
		t.Errorf("Content-Type = %q, want a problem document", ct)
	}
}

// The tier that actually keeps the process alive. A rate limiter bounds
// arrivals and says nothing about how many are still running.
func TestInflightShedsBeyondTheLimit(t *testing.T) {
	t.Parallel()

	const limit = 3
	release := make(chan struct{})
	entered := make(chan struct{}, limit)

	srv := httptest.NewServer(ratelimit.Inflight(limit)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			entered <- struct{}{}
			<-release
			w.WriteHeader(http.StatusOK)
		})))
	defer srv.Close()

	var wg sync.WaitGroup
	for range limit {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := get(t, srv)
			_ = resp.Body.Close()
		}()
	}
	for range limit {
		<-entered
	}

	// Every slot is held, so this one is shed rather than queued: a queue in
	// front of an overloaded server turns a fast failure into a slow one.
	shed := get(t, srv)
	defer shed.Body.Close() //nolint:errcheck // test response

	close(release)
	wg.Wait()

	if shed.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", shed.StatusCode)
	}
	if shed.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After: the client is refused but not told when to come back")
	}
}

func get(t *testing.T, srv *httptest.Server) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/tasks", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return resp
}
