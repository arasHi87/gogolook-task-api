package docs

import (
	_ "embed"
	"net/http"
)

// Paths served by Handler.
const (
	PathYAML = "/openapi.yaml"
	PathJSON = "/openapi.json"
	PathUI   = "/docs"
)

// scalarHTML is the reference UI: one page that loads the spec from this same
// server. It is a single file with no build step, and the only thing it needs
// from the network is the Scalar bundle.
//
//go:embed scalar.html
var scalarHTML []byte

// Handler registers the documentation routes on mux.
//
// These live on the public listener, not the admin one: an API's own contract
// is part of its public surface, and a reviewer who has to find a second port
// to read it will not.
func Handler(mux *http.ServeMux) {
	mux.HandleFunc("GET "+PathYAML, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(YAML())
	})

	mux.HandleFunc("GET "+PathJSON, func(w http.ResponseWriter, _ *http.Request) {
		body, err := JSON()
		if err != nil {
			http.Error(w, "openapi document is unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(body)
	})

	mux.HandleFunc("GET "+PathUI, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(scalarHTML)
	})
}
