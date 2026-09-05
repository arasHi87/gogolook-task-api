package handler

import (
	"context"
	"net/http"
)

// Rewind exposes the retry's body-rewind to the test package.
//
// The behaviour it guards cannot be reproduced reliably from outside: net/http
// re-sends on a fresh connection by calling GetBody itself, so whether a
// drained body is rescued depends on whether the previous connection happened
// to be reusable. Testing the helper directly is the only deterministic way to
// pin it.
func Rewind(ctx context.Context, req *http.Request) (*http.Request, error) {
	return rewind(ctx, req)
}
