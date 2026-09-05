package runtime

import "net/http"

// NewHandlerAround builds the production middleware chain around an arbitrary
// handler. It exists so a test can assert on how the chain behaves when the
// thing it wraps panics or misbehaves, without reaching into unexported state
// and without adding a seam to the exported API.
func NewHandlerAround(o Options, h http.Handler) (http.Handler, error) {
	o.handler = h
	return NewHandler(o)
}
