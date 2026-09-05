package app

import (
	"os"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/logging"
)

func newApp(t *testing.T, mode Mode) *App {
	t.Helper()

	cfg := config.Defaults()
	log, err := logging.New(logging.Options{Level: "error", Format: "json", Writer: os.Stderr})
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	a, err := New(Options{
		Mode:   mode,
		Config: &cfg,
		Flags:  pflag.NewFlagSet("test", pflag.ContinueOnError),
		Logger: log,
	})
	if err != nil {
		t.Fatalf("New(%s): %v", mode, err)
	}
	return a
}

func workerNames(a *App) []string {
	var names []string
	for _, w := range a.workers() {
		names = append(names, w.Name)
	}
	return names
}

// The table is the shutdown sequence, so its order is the contract. Each
// position here is a decision, and a reordering that looks harmless is how a
// graceful drain stops being graceful.
func TestWorkerTableIsInDrainOrder(t *testing.T) {
	t.Parallel()

	cases := map[Mode][]string{
		// readiness first: a load balancer stops routing here before the
		// listener stops accepting, so requests arriving in that window are
		// still served rather than refused.
		//
		// admin last: health and metrics answer for the whole drain instead of
		// going dark at the start of it.
		//
		// idempotency-purge is present in every mode: it is leader-elected, so
		// running it everywhere costs nothing and means keys keep being purged
		// even when only workers are up.
		ModeServe:  {"readiness", "api", "idempotency-purge", "config-reloader", "admin"},
		ModeAll:    {"readiness", "api", "idempotency-purge", "config-reloader", "admin"},
		ModeWorker: {"readiness", "idempotency-purge", "config-reloader", "admin"},
	}

	for mode, want := range cases {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			got := workerNames(newApp(t, mode))
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("workers() = %v, want %v", got, want)
			}
		})
	}
}

// A worker process serves no public port, but it still has to be scrapeable
// and probeable, so the admin listener runs in every mode.
func TestAdminListenerRunsInEveryMode(t *testing.T) {
	t.Parallel()

	for _, mode := range []Mode{ModeServe, ModeWorker, ModeAll} {
		a := newApp(t, mode)
		if a.adminServer == nil {
			t.Errorf("%s has no admin listener", mode)
		}
		if got := (mode.Runs()); (a.apiServer != nil) != got {
			t.Errorf("%s: apiServer present = %v, want %v", mode, a.apiServer != nil, got)
		}
	}
}

// Stopping the readiness worker is what takes the replica out of rotation.
func TestReadinessWorkerFlipsOnStop(t *testing.T) {
	t.Parallel()

	a := newApp(t, ModeAll)
	readiness := a.workers()[0]

	if a.health.Draining() {
		t.Fatal("draining before shutdown")
	}
	if err := readiness.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !a.health.Draining() {
		t.Error("stopping the readiness worker did not flip readiness")
	}
}

// The two listeners must never share a port, or the admin surface — pprof
// included — becomes reachable from the internet.
func TestListenersUseDifferentPorts(t *testing.T) {
	t.Parallel()

	a := newApp(t, ModeAll)
	if a.apiServer.Addr == a.adminServer.Addr {
		t.Fatalf("both listeners are on %s", a.apiServer.Addr)
	}
	cfg := a.Config()
	if a.adminServer.Addr != cfg.Admin.Addr {
		t.Errorf("admin listener on %s, want %s", a.adminServer.Addr, cfg.Admin.Addr)
	}
}
