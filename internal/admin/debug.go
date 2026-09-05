package admin

import (
	"io"
	"net/http"
	"net/http/pprof"
	"strings"
)

// Debug is the operator surface: what this process thinks its configuration is,
// what it is logging, and what its goroutines are doing.
//
// None of it is optional-nice-to-have. The three questions asked at three in
// the morning are "what config is it actually running", "can I see more without
// restarting it", and "what is it stuck on", and a process that cannot answer
// them is one you restart and lose the evidence from.
//
// All of it is on the admin listener, never the public one. A heap profile is a
// memory dump, and pprof on a public port hands one to anyone who can reach it.
type Debug struct {
	// Config writes the effective configuration, with secrets masked.
	Config func(w io.Writer) error
	// Level reads and writes the log level at runtime.
	Level LevelKnob
	// Metrics is the Prometheus handler, or nil when metrics are disabled.
	Metrics http.Handler
	// Pprof enables the profiling endpoints.
	Pprof bool
}

// LevelKnob is the runtime log level.
type LevelKnob interface {
	LevelString() string
	SetLevel(name string) error
}

// register mounts the debug surface on the admin mux.
func (h *Handler) register(mux *http.ServeMux, d Debug) {
	if d.Metrics != nil {
		mux.Handle("GET /metrics", d.Metrics)
	}
	if d.Config != nil {
		mux.HandleFunc("GET /debug/config", configHandler(d.Config))
	}
	if d.Level != nil {
		mux.HandleFunc("GET /debug/log-level", getLevel(d.Level))
		mux.HandleFunc("PUT /debug/log-level", setLevel(d.Level))
	}
	if d.Pprof {
		registerPprof(mux)
	}
}

// configHandler dumps the merged configuration.
//
// It is the answer to "which config file did it actually read", which is a
// different question from "what is in the config file" and is the one that is
// usually wrong. Secrets are masked by the writer, not here: masking at the
// edge means every other consumer of the same dump is also safe.
func configHandler(write func(io.Writer) error) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Cache-Control", "no-store")
		if err := write(w); err != nil {
			// The header is already sent, so there is no status left to
			// change. Appending the error is the only way to say anything.
			_, _ = w.Write([]byte("\n# error rendering configuration: " + err.Error() + "\n"))
		}
	}
}

func getLevel(knob LevelKnob) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"level": knob.LevelString()})
	}
}

// setLevel turns the verbosity up without a restart.
//
// The alternative is restarting the process to debug it, which discards the
// state that was about to explain the problem. A plain text body rather than
// JSON, so it is one curl:
//
//	curl -X PUT --data debug localhost:9090/debug/log-level
func setLevel(knob LevelKnob) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "could not read the body"})
			return
		}

		want := strings.TrimSpace(string(body))
		if want == "" {
			want = strings.TrimSpace(r.URL.Query().Get("level"))
		}
		if err := knob.SetLevel(want); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"level": knob.LevelString()})
	}
}

// registerPprof mounts the profiling endpoints.
//
// Mounted explicitly rather than by importing net/http/pprof for its side
// effect on http.DefaultServeMux. The import-for-side-effect idiom registers
// them on a global mux, and anything that later serves that mux — a dependency,
// a copied snippet — exposes them wherever it is listening. Naming the routes
// here is what keeps them on this port.
func registerPprof(mux *http.ServeMux) {
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("POST /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
}
