// Command webhook-sink is a deliberately unreliable receiver.
//
// It exists so the resilience claims can be demonstrated rather than asserted:
// point the worker at it, tell it to fail, and watch the retries, the backoff
// and (from M8) the circuit breaker in the dashboards.
//
// It also records what it received, so "exactly once" can be checked rather
// than believed.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"sync"
	"time"
)

type sink struct {
	mu sync.Mutex
	// seen counts deliveries per X-Event-Id, which is what makes duplicate
	// delivery visible: at-least-once means this can exceed one, and the
	// receiver is expected to deduplicate on it.
	seen     map[string]int
	received int

	failRate float64
	latency  time.Duration
	down     bool
}

func main() {
	s := &sink{
		seen:     map[string]int{},
		failRate: envFloat("SINK_FAIL_RATE", 0),
		latency:  envDuration("SINK_LATENCY", 0),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /hook", s.receive)
	mux.HandleFunc("GET /stats", s.stats)
	mux.HandleFunc("POST /control", s.control)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("webhook sink listening on :8080 (fail_rate=%.2f latency=%s)", s.failRate, s.latency)
	log.Fatal(srv.ListenAndServe())
}

func (s *sink) receive(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	latency, failRate, down := s.latency, s.failRate, s.down
	s.mu.Unlock()

	if latency > 0 {
		select {
		case <-time.After(latency):
		case <-r.Context().Done():
			return
		}
	}

	if down {
		http.Error(w, "sink is down", http.StatusServiceUnavailable)
		return
	}
	if failRate > 0 && rand.Float64() < failRate {
		http.Error(w, "injected failure", http.StatusInternalServerError)
		return
	}

	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	s.mu.Lock()
	s.received++
	s.seen[r.Header.Get("X-Event-Id")]++
	s.mu.Unlock()

	log.Printf("received event=%v id=%s attempt-key=%s",
		body["event"], r.Header.Get("X-Event-Id"), r.Header.Get("Idempotency-Key"))
	w.WriteHeader(http.StatusOK)
}

func (s *sink) stats(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	duplicates := 0
	for _, n := range s.seen {
		if n > 1 {
			duplicates += n - 1
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"received":   s.received,
		"distinct":   len(s.seen),
		"duplicates": duplicates,
		"fail_rate":  s.failRate,
		"latency":    s.latency.String(),
		"down":       s.down,
		"by_event":   s.seen,
	})
}

// control changes the sink's behaviour at runtime, so a demo can break a
// dependency without restarting anything.
func (s *sink) control(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FailRate *float64 `json:"fail_rate"`
		Latency  *string  `json:"latency"`
		Down     *bool    `json:"down"`
		Reset    bool     `json:"reset"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if req.FailRate != nil {
		s.failRate = *req.FailRate
	}
	if req.Latency != nil {
		d, err := time.ParseDuration(*req.Latency)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.latency = d
	}
	if req.Down != nil {
		s.down = *req.Down
	}
	if req.Reset {
		s.seen = map[string]int{}
		s.received = 0
	}

	fmt.Fprintf(w, "fail_rate=%.2f latency=%s down=%t\n", s.failRate, s.latency, s.down)
}

func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var f float64
	if _, err := fmt.Sscanf(v, "%f", &f); err != nil {
		return def
	}
	return f
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
