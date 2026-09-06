package harness

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// repoRoot is where the module lives, relative to a package under test/.
const repoRoot = "../.."

// binary is the compiled taskapi, built once for the whole test binary by
// TestMain.
var binary string

// TestMain builds the server under test and cleans up after it.
//
// A consumer's whole main_test.go is one line:
//
//	func TestMain(m *testing.M) { harness.TestMain(m) }
//
// It lives here rather than being copied into every scenario package, and it
// is a function rather than lazy initialisation because the binary needs
// deleting afterwards and a package has no other hook for that.
//
// Built rather than `go run`: a run wrapper is a second process between the
// test and the server, and these scenarios send signals to the server. Sending
// SIGKILL to a wrapper proves nothing about how the server dies.
func TestMain(m *testing.M) {
	// testing.Short reads a flag, so the flags have to be parsed before it can
	// be asked. m.Run would do it, but by then the build has already happened.
	flag.Parse()

	if testing.Short() {
		// Every scenario skips itself in short mode; there is nothing to build.
		os.Exit(m.Run())
	}

	dir, err := os.MkdirTemp("", "taskapi-e2e-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "harness: temp dir: %v\n", err)
		os.Exit(1)
	}

	binary = filepath.Join(dir, "taskapi")
	if err := build(binary); err != nil {
		fmt.Fprintf(os.Stderr, "harness: %v\n", err)
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

	// The compiler, on a path this package chose, with no context because
	// TestMain has none and a build that hangs is a build that fails.
	//nolint:gosec,noctx // G204: a fixed argv; noctx: TestMain has no context
	cmd := exec.Command("go", args...)
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("build taskapi: %w\n%s", err, out)
	}
	return nil
}
