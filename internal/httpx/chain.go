package httpx

import "net/http"

// Middleware is one link in the chain.
type Middleware func(http.Handler) http.Handler

// Chain composes middleware so that the first argument is the outermost — the
// order they are written is the order a request passes through them.
//
//	Chain(h, Recover(), RequestID())
//
// runs Recover, then RequestID, then h. Reading top to bottom gives the right
// answer, which is the only property that matters here.
func Chain(h http.Handler, mw ...Middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}
