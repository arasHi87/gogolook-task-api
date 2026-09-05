// Command taskapi is the whole service: HTTP API, queue worker, migrations and
// the health probe used by the container image.
//
// One binary, several subcommands. Running the API and the queue consumer as
// separate processes from the same image is what demonstrates that the queue is
// genuinely decoupled — and it makes "kill the worker mid-job and watch the
// lease reaper" a live demo rather than a claim.
package main

import (
	"os"

	"github.com/arasHi87/gogolook-task-api/internal/buildinfo"
	"github.com/arasHi87/gogolook-task-api/internal/cli"
)

// Stamped in by -ldflags at build time; see the Makefile and the Dockerfile.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	buildinfo.Set(version, commit, date)

	// cobra has already printed the error and, for a usage error, the usage.
	// Exiting quietly here keeps the message from appearing twice.
	if err := cli.NewRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}
