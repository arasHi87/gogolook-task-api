package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// process is one taskapi under test: the real binary, started the way the
// container starts it, with its log stream parsed rather than discarded.
//
// Parsing the log is what makes the harness deterministic. The server logs its
// resolved listen address, so the test never has to guess a free port and race
// something else for it; it logs "expired leases reclaimed", so a crash test
// can wait for the reaper instead of sleeping past it. A harness that sleeps
// for "long enough" is the harness everybody eventually marks as flaky.
type process struct {
	t    *testing.T
	name string
	cmd  *exec.Cmd

	stdout bytes.Buffer

	mu      sync.Mutex
	records []record
	waiters []*waiter

	// exited is closed once Wait has returned, so stop and kill can tell the
	// difference between a process that is draining and one that is gone.
	exited  chan struct{}
	exitErr error

	signalled bool
}

// record is one decoded log line. Lines that are not JSON — a panic trace, a
// runtime message — are kept under the "raw" key so they still reach the dump.
type record map[string]any

func (r record) str(key string) string {
	s, _ := r[key].(string)
	return s
}

// waiter is a predicate someone is blocked on.
type waiter struct {
	match func(record) bool
	ch    chan record
}

// startProcess launches the binary and returns once it is running. It does not
// wait for readiness; the caller decides what "ready" means for its mode.
func startProcess(t *testing.T, name string, args []string, env []string) *process {
	t.Helper()

	p := &process{
		t:      t,
		name:   name,
		cmd:    exec.Command(binary, args...),
		exited: make(chan struct{}),
	}
	p.cmd.Env = append(cleanEnv(), env...)
	p.cmd.Stdout = &p.stdout

	stderr, err := p.cmd.StderrPipe()
	if err != nil {
		t.Fatalf("%s: stderr pipe: %v", name, err)
	}
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("%s: start: %v", name, err)
	}

	var scanning sync.WaitGroup
	scanning.Add(1)
	go func() {
		defer scanning.Done()
		p.consume(stderr)
	}()
	go func() {
		// Wait only after the pipe is drained: calling it earlier closes the
		// pipe and truncates the very log lines a failure needs.
		scanning.Wait()
		p.finish(p.cmd.Wait())
	}()

	t.Cleanup(func() {
		// The error is the process's own exit status, which stop already
		// reports through t when it matters. A test that cares about how this
		// one exited calls stop itself and checks the result.
		_ = p.stop(10 * time.Second)
		if t.Failed() {
			p.dump()
		}
	})
	return p
}

// cleanEnv is the parent environment with every TASKAPI_ variable removed.
//
// Without this the suite reads the developer's shell: a TASKAPI_LOGGING_LEVEL
// exported for a debugging session earlier in the day silently changes what
// the harness can wait for, and the failure looks like a product bug.
func cleanEnv() []string {
	env := os.Environ()
	kept := env[:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "TASKAPI_") {
			kept = append(kept, kv)
		}
	}
	return kept
}

// consume reads the log stream, decodes it, and wakes anyone waiting on it.
func (p *process) consume(r io.Reader) {
	sc := bufio.NewScanner(r)
	// Log lines are short, but a panic trace on one line is not.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}

		rec := record{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			rec = record{"raw": line}
		}
		p.publish(rec)
	}
}

// publish appends a record and delivers it to every waiter it satisfies.
func (p *process) publish(rec record) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.records = append(p.records, rec)

	kept := p.waiters[:0]
	for _, w := range p.waiters {
		if w.match(rec) {
			w.ch <- rec
			close(w.ch)
			continue
		}
		kept = append(kept, w)
	}
	p.waiters = kept
}

// await blocks until a log record matches, and fails the test if none does.
//
// It checks the records already seen before registering, so a caller that asks
// for something the process logged during startup is not left waiting for it
// to happen a second time.
func (p *process) await(match func(record) bool, within time.Duration, what string) record {
	p.t.Helper()

	p.mu.Lock()
	for _, rec := range p.records {
		if match(rec) {
			p.mu.Unlock()
			return rec
		}
	}
	w := &waiter{match: match, ch: make(chan record, 1)}
	p.waiters = append(p.waiters, w)
	p.mu.Unlock()

	select {
	case rec := <-w.ch:
		return rec
	case <-p.exited:
		p.dump()
		p.t.Fatalf("%s: exited (%v) while waiting for %s", p.name, p.exitErr, what)
	case <-time.After(within):
		p.dump()
		p.t.Fatalf("%s: timed out after %s waiting for %s", p.name, within, what)
	}
	return nil
}

// addr returns the address a named listener actually bound.
//
// The harness starts every listener on port 0 and reads the result back from
// this line. Picking a free port in the test and handing it over would leave a
// window in which anything else on the machine can take it, which is a class
// of flake that only ever shows up in CI.
func (p *process) addr(listener string) string {
	p.t.Helper()

	rec := p.await(func(r record) bool {
		return r.str("msg") == "listening" && r.str("listener") == listener
	}, 30*time.Second, "the "+listener+" listener")

	return rec.str("addr")
}

// signal sends a signal without waiting for the consequences.
func (p *process) signal(sig syscall.Signal) {
	p.t.Helper()

	p.mu.Lock()
	p.signalled = true
	p.mu.Unlock()

	if err := p.cmd.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
		p.t.Fatalf("%s: signal %s: %v", p.name, sig, err)
	}
}

// stop drains the process the way an orchestrator does: SIGTERM, then wait.
//
// A process that outlives the grace period is killed and the test is told,
// because "shutdown took longer than the grace period" is a finding, not
// something to paper over.
func (p *process) stop(grace time.Duration) error {
	p.t.Helper()

	select {
	case <-p.exited:
		return p.exitErr
	default:
	}

	p.signal(syscall.SIGTERM)
	select {
	case <-p.exited:
		return p.exitErr
	case <-time.After(grace):
		_ = p.cmd.Process.Kill()
		<-p.exited
		p.t.Errorf("%s: did not exit within %s of SIGTERM", p.name, grace)
		return p.exitErr
	}
}

// kill is a crash: no drain, no chance to release a lease, nothing written on
// the way out. It is the only honest way to test the reaper.
func (p *process) kill() {
	p.t.Helper()

	p.signal(syscall.SIGKILL)
	select {
	case <-p.exited:
	case <-time.After(10 * time.Second):
		p.t.Fatalf("%s: still alive 10s after SIGKILL", p.name)
	}
}

// running reports whether the process is still up.
func (p *process) running() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

// finish records the exit and releases everyone waiting on this process.
func (p *process) finish(err error) {
	p.mu.Lock()
	// A signalled process exits non-zero by design; that is not a failure.
	if err != nil && !p.signalled {
		p.exitErr = err
	}
	for _, w := range p.waiters {
		close(w.ch)
	}
	p.waiters = nil
	p.mu.Unlock()

	close(p.exited)
}

// dump writes the process log to the test output. It runs on failure only:
// on a green run this is a megabyte of noise, and on a red one it is the
// entire diagnosis.
func (p *process) dump() {
	p.mu.Lock()
	records := append([]record(nil), p.records...)
	stdout := p.stdout.String()
	p.mu.Unlock()

	var b strings.Builder
	for _, rec := range records {
		line, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if stdout != "" {
		b.WriteString("--- stdout ---\n")
		b.WriteString(stdout)
	}
	p.t.Logf("=== %s log ===\n%s", p.name, b.String())
}
