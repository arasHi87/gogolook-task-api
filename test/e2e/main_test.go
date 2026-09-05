// Package e2e is the live end-to-end suite: the real binary, a real database,
// real processes, real HTTP.
//
// Everything else in the tree tests a part. This tests the system, and it is
// deliberately built out of the same pieces the deployment is:
//
//   - the binary that ships, started the way the container starts it, with its
//     configuration arriving through the environment;
//   - two processes, `serve` and `worker`, which is the split
//     deploy/compose.yaml runs — so nothing here can pass by accident of two
//     halves sharing a heap;
//   - a real Postgres, migrated, one private database per test;
//   - a webhook receiver that records what arrived and can be told to fail.
//
// What that buys over the integration suite is the seams: flag parsing, the
// four-layer config merge, signal handling, the drain, and the fact that a job
// enqueued by one process is claimed and run by another. Those are exactly the
// places where a change breaks the system while every unit test stays green.
//
// It costs a Docker daemon and about a minute. `go test -short` skips it, so
// `task test` is unaffected and `task e2e` is the way in.
package e2e

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// repoRoot is where the module lives, relative to this package. Every `go
// build` below runs from there.
const repoRoot = "../.."

// binary is the compiled taskapi, built once for the whole test binary.
//
// Built rather than `go run`: a run wrapper is a second process between the
// test and the server, and this suite sends signals to the server. Sending
// SIGKILL to a wrapper proves nothing about how the server dies.
var binary string

func TestMain(m *testing.M) {
	// testing.Short reads a flag, so the flags have to be parsed before it can
	// be asked. m.Run would do it, but by then the build has already happened.
	flag.Parse()

	if testing.Short() {
		// Every test skips itself in short mode; there is nothing to build.
		os.Exit(m.Run())
	}

	dir, err := os.MkdirTemp("", "taskapi-e2e-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: temp dir: %v\n", err)
		os.Exit(1)
	}

	binary = filepath.Join(dir, "taskapi")
	if err := build(binary); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// build compiles the server under test.
//
// It inherits -race from the test binary. Without that, `go test -race ./...`
// would race-check the harness and leave the server — the part that actually
// runs eight worker goroutines against a shared pool — uninstrumented, which
// is the wrong way round.
func build(out string) error {
	args := []string{"build", "-o", out}
	if raceEnabled {
		args = append(args, "-race")
	}
	args = append(args, "./cmd/taskapi")

	cmd := exec.Command("go", args...)
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("build taskapi: %w\n%s", err, out)
	}
	return nil
}
