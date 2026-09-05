// Package admin is the private listener: health, readiness and, from M9,
// metrics and the debug endpoints.
//
// It is a separate port from the API on purpose. Nothing here should ever be
// reachable from the internet, and a separate listener makes that a property
// of the deployment rather than a rule someone has to remember when adding a
// route.
package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/buildinfo"
)

// Check is one readiness probe contributed by a subsystem.
//
// Probes are registered at construction and never change, so serving them
// needs no lock: the only mutable state here is the draining flag.
type Check struct {
	// Name appears in the readiness response, so an operator can see which
	// dependency is the problem without reading logs.
	Name string
	// Probe reports whether the subsystem can serve. It must respect the
	// context: a probe that hangs turns readiness into an outage of its own.
	Probe func(ctx context.Context) error
}

// probeTimeout bounds the whole readiness response. A probe that is slow is a
// probe that is failing, as far as a load balancer is concerned.
const probeTimeout = 2 * time.Second

// Handler serves the admin endpoints.
type Handler struct {
	checks   []Check
	draining atomic.Bool
	started  time.Time
	now      func() time.Time
}

// New returns a handler that reports ready once every check passes.
func New(checks ...Check) *Handler {
	h := &Handler{checks: checks, now: time.Now}
	h.started = h.now()
	return h
}

// StartDraining flips readiness to not-ready.
//
// It is called first in the shutdown sequence, before the API listener stops
// accepting. That ordering is the whole point: a load balancer needs a moment
// to notice and stop routing, and the requests that arrive in that moment
// should still be served rather than refused.
func (h *Handler) StartDraining() { h.draining.Store(true) }

// Draining reports whether shutdown has begun.
func (h *Handler) Draining() bool { return h.draining.Load() }

// Mux returns the admin routes: health, readiness, version, and whatever of
// the debug surface d turns on.
func (h *Handler) Mux(d Debug) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("GET /readyz", h.readyz)
	mux.HandleFunc("GET /version", h.version)
	h.register(mux, d)
	return mux
}

// healthz answers whether the process is alive, and nothing else.
//
// It deliberately does not check the database. Liveness failures get a
// container killed and restarted; if a shared database goes down, every
// replica would be killed at once and would come back into a thundering herd
// against the thing that was already struggling. Dependency health is
// readiness, which takes a replica out of rotation without restarting it.
func (h *Handler) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"uptime": h.now().Sub(h.started).Round(time.Second).String(),
	})
}

// readyz answers whether this replica should receive traffic.
func (h *Handler) readyz(w http.ResponseWriter, r *http.Request) {
	if h.draining.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "draining"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	results := make(map[string]string, len(h.checks))
	ready := true
	for _, c := range h.checks {
		if err := c.Probe(ctx); err != nil {
			// The failing dependency's name is useful; its error text may name
			// a host or a credential, so it does not leave the process.
			results[c.Name] = "failed"
			ready = false
			continue
		}
		results[c.Name] = "ok"
	}

	status, label := http.StatusOK, "ready"
	if !ready {
		status, label = http.StatusServiceUnavailable, "not ready"
	}
	writeJSON(w, status, map[string]any{"status": label, "checks": results})
}

func (h *Handler) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, buildinfo.Get())
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
