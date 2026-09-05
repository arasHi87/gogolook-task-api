// Package runtime is how the service is served.
//
// It owns the transport: the route table, the REST-to-Connect transcoding, the
// JSON wire format and the middleware chain. It knows nothing about what a
// task is — internal/api holds the handler, internal/task holds the rules.
//
// The split is what makes the transport replaceable. If vanguard ever has to
// go, this package is the only one that changes.
package runtime

import (
	"errors"
	"fmt"
	"net/http"

	"connectrpc.com/connect"
	"connectrpc.com/validate"
	"connectrpc.com/vanguard"

	"github.com/arasHi87/gogolook-task-api/docs"
	"github.com/arasHi87/gogolook-task-api/gen/task/v1/taskv1connect"
	"github.com/arasHi87/gogolook-task-api/internal/api"
	"github.com/arasHi87/gogolook-task-api/internal/auth"
	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/httpx"
	"github.com/arasHi87/gogolook-task-api/internal/idempotency"
	"github.com/arasHi87/gogolook-task-api/internal/ratelimit"
	"github.com/arasHi87/gogolook-task-api/internal/task"
)

// Options configures the public HTTP surface.
type Options struct {
	// Service is the domain.
	Service *task.Service
	// MaxBodyBytes caps request bodies. Zero disables the cap.
	MaxBodyBytes int64
	// Idempotency remembers what a key produced. Nil disables the layer, which
	// is what a deployment with no store configured gets.
	Idempotency idempotency.Store
	// IdempotencyConfig tunes the layer. Ignored when Idempotency is nil.
	IdempotencyConfig config.Idempotency

	// Auth resolves the caller to a client id and a tier. It always runs: the
	// rate limiter keys on the identity, and an absent identity means every
	// anonymous caller shares one bucket.
	Auth *auth.Resolver
	// RateLimit is the per-caller token bucket. Nil disables both tiers.
	RateLimit *ratelimit.Limiter
	// GlobalInflight caps concurrent requests. Zero disables the semaphore.
	GlobalInflight int

	// handler replaces the transcoder. Unexported because it exists only so a
	// test can drive the real middleware chain around a handler that misbehaves
	// on purpose; there is no way to set it from outside the package.
	handler http.Handler
}

// NewHandler builds the public handler: the REST routes from the
// specification, the native Connect and gRPC surface, and the API
// documentation.
//
// One mux, one port, one handler implementation. vanguard reads the same
// google.api.http annotations the OpenAPI document was generated from and
// transcodes REST onto the Connect handler, so what is served and what is
// documented come from one source and cannot drift.
func NewHandler(o Options) (http.Handler, error) {
	if o.Service == nil {
		return nil, errors.New("runtime: a task service is required")
	}

	routes, err := o.routes()
	if err != nil {
		return nil, err
	}
	return wrap(routes, o), nil
}

// routes assembles the handler tree, before any middleware.
func (o Options) routes() (http.Handler, error) {
	root := o.handler // test seam; nil in every real caller
	if root == nil {
		var err error
		if root, err = o.transcoder(); err != nil {
			return nil, err
		}
	}

	mux := http.NewServeMux()
	docs.Handler(mux)
	// Everything else — the eight REST routes and the Connect procedures — is
	// routed from the proto annotations.
	mux.Handle("/", root)
	return mux, nil
}

// transcoder builds the Connect handler and the REST transcoding in front of
// it.
func (o Options) transcoder() (http.Handler, error) {
	// protovalidate enforces the constraints declared in the proto: the status
	// enum, the name length, the uuid format. Declared once, checked here, and
	// published in the OpenAPI — no hand-written validation to fall out of step.
	path, handler := taskv1connect.NewTaskServiceHandler(
		api.NewServer(o.Service),
		connect.WithInterceptors(validate.NewInterceptor()),
		connect.WithCodec(connectJSONCodec{}),
	)

	t, err := vanguard.NewTranscoder(
		[]*vanguard.Service{vanguard.NewService(path, handler)},
		vanguard.WithCodec(newJSONCodec),
	)
	if err != nil {
		return nil, fmt.Errorf("runtime: build transcoder: %w", err)
	}
	return t, nil
}

// wrap puts the middleware chain around the routes.
//
// Outermost first, and the order is load-bearing:
//
//	RequestID   before everything, so the id exists for every log line and
//	            every error body — including the ones Recover writes.
//	Logger      outside Recover, so a recovered panic is still recorded as the
//	            500 it became rather than as a request with no outcome.
//	Recover     inside both, so a panic in any middleware below it or in the
//	            handler produces a response instead of a dropped connection.
//	Deprecation before the handler, because headers must be set before anything
//	            writes.
//	Inflight    as early as work can be refused. Before Auth on purpose: the
//	            point of load shedding is to be the cheapest possible
//	            rejection, and hashing a token first is work done for a
//	            request that is about to be thrown away. Which tenant is
//	            flooding is the per-client limiter's question, not this one's.
//	Auth        before the limiter, which needs the tier and the bucket key,
//	            and before the handler, so every log line carries the client.
//	RateLimit   after Auth, because the quota is per tier.
//	MaxBody     before Idempotency, so the body that gets buffered and hashed
//	            is already size-capped.
//	Idempotency innermost, so what it stores is the response the handler
//	            produced rather than one a later middleware decorated — and so
//	            a replay still passes back out through the logger and comes
//	            out of the chain looking like any other response.
func wrap(h http.Handler, o Options) http.Handler {
	mw := []httpx.Middleware{
		httpx.RequestID(),
		httpx.Logger(),
		httpx.Recover(),
	}
	if o.GlobalInflight > 0 {
		mw = append(mw, ratelimit.Inflight(o.GlobalInflight))
	}
	if o.Auth != nil {
		mw = append(mw, auth.Middleware(o.Auth))
	}
	if o.RateLimit != nil {
		mw = append(mw, ratelimit.Middleware(o.RateLimit))
	}

	mw = append(mw, httpx.Deprecation(), httpx.MaxBody(o.MaxBodyBytes))
	if o.Idempotency != nil {
		mw = append(mw, idempotency.Middleware(o.Idempotency, o.IdempotencyConfig))
	}
	return httpx.Chain(h, mw...)
}
