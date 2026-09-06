// Package e2e is the live end-to-end suite: the real binary, a real database,
// real processes, real HTTP.
//
// Everything else in the tree tests a part. These test the system, and each
// file is one theme:
//
//	contract_test.go       the four endpoints the assignment asks for
//	delivery_test.go       a write becomes an event becomes a webhook
//	idempotency_test.go    the same request twice happens once
//	resilience_test.go     crashes, drains, and a dependency that goes away
//	guards_test.go         the rate limiter and the circuit breaker
//	observability_test.go  what answers on which port, and what it says
//
// The verbs come from test/harness, whose package documentation has the
// architecture diagram and the three decisions that make these deterministic.
// A scenario should read as a sequence of events; if it reads as a sequence of
// assertions, the missing verb belongs in the harness.
//
// It costs a Docker daemon and about a minute. `go test -short` skips it, so
// `task test` is unaffected and `task e2e` is the way in.
package e2e

import (
	"testing"

	"github.com/arasHi87/gogolook-task-api/test/harness"
)

func TestMain(m *testing.M) { harness.TestMain(m) }
