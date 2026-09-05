package metrics

import "github.com/prometheus/client_golang/prometheus"

// Guards is the inbound protection and the outbound one: who was let in, and
// what the circuit is doing.
type Guards struct {
	rateLimit  *prometheus.CounterVec
	activeKeys *prometheus.GaugeVec
	shed       prometheus.Counter
	authz      *prometheus.CounterVec

	breakerState       *prometheus.GaugeVec
	breakerTransitions *prometheus.CounterVec
	breakerCalls       *prometheus.CounterVec
}

// Breaker states, as numbers, because a gauge cannot hold a word.
//
// A state-timeline panel reads these directly, which is why the encoding is
// ordered by severity rather than alphabetically: higher is worse, so a
// max-over-time is still meaningful.
const (
	BreakerClosed   = 0
	BreakerHalfOpen = 1
	BreakerOpen     = 2
)

func newGuards(reg prometheus.Registerer) *Guards {
	g := &Guards{
		// tier is three values and decision is two. The key itself — a client
		// id or an address — is deliberately absent: it is the caller's to
		// choose, which makes it the one label an attacker could grow.
		rateLimit: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "ratelimit_decisions_total",
			Help:      "Rate-limit decisions, by tier.",
		}, []string{"tier", "decision"}),

		// The size of the bucket map, which is the thing that would grow
		// without bound if the eviction ever broke.
		activeKeys: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "ratelimit_active_keys",
			Help:      "Rate-limit buckets currently held.",
		}, []string{"scope"}),

		shed: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "http_server_inflight_rejected_total",
			Help:      "Requests shed by the in-flight limiter.",
		}),

		authz: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "auth_decisions_total",
			Help:      "Caller resolution outcomes.",
		}, []string{"result"}),

		breakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "circuit_breaker_state",
			Help:      "Circuit state: 0 closed, 1 half-open, 2 open.",
		}, []string{"name"}),

		breakerTransitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "circuit_breaker_transitions_total",
			Help:      "Circuit state changes.",
		}, []string{"name", "from", "to"}),

		breakerCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "circuit_breaker_calls_total",
			Help:      "Calls through the circuit, by result.",
		}, []string{"name", "result"}),
	}

	reg.MustRegister(
		g.rateLimit, g.activeKeys, g.shed, g.authz,
		g.breakerState, g.breakerTransitions, g.breakerCalls,
	)
	return g
}

// RateLimited records one decision.
func (g *Guards) RateLimited(tier string, allowed bool) {
	decision := "throttled"
	if allowed {
		decision = "allowed"
	}
	g.rateLimit.WithLabelValues(tier, decision).Inc()
}

// ActiveKeys reports how many buckets are held.
func (g *Guards) ActiveKeys(n int) { g.activeKeys.WithLabelValues("client").Set(float64(n)) }

// Shed records a request refused by the in-flight limiter.
func (g *Guards) Shed() { g.shed.Inc() }

// Authenticated records how a caller was resolved.
func (g *Guards) Authenticated(result string) { g.authz.WithLabelValues(result).Inc() }

// BreakerState records the circuit's state, and the transition that produced
// it.
//
// Both, because they answer different questions: the gauge says what is true
// now and draws the timeline, the counter says how often it has flapped. A
// circuit that opens and closes twelve times an hour looks identical to a
// healthy one in the gauge if the scrape lands between transitions.
func (g *Guards) BreakerState(name, from, to string, state float64) {
	g.breakerState.WithLabelValues(name).Set(state)
	g.breakerTransitions.WithLabelValues(name, from, to).Inc()
}

// BreakerCall records one call through the circuit.
func (g *Guards) BreakerCall(name, result string) {
	g.breakerCalls.WithLabelValues(name, result).Inc()
}
