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
	"github.com/arasHi87/gogolook-task-api/internal/httpx"
	"github.com/arasHi87/gogolook-task-api/internal/task"
)

// Options configures the public HTTP surface.
type Options struct {
	// Service is the domain.
	Service *task.Service
	// MaxBodyBytes caps request bodies. Zero disables the cap.
	MaxBodyBytes int64

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
	return wrap(routes, o.MaxBodyBytes), nil
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
//	MaxBody     innermost, closest to whatever reads the body.
func wrap(h http.Handler, maxBody int64) http.Handler {
	return httpx.Chain(h,
		httpx.RequestID(),
		httpx.Logger(),
		httpx.Recover(),
		httpx.Deprecation(),
		httpx.MaxBody(maxBody),
	)
}
