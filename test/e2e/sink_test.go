package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/task/pgrepo"
)

// sink is the webhook receiver: a real HTTP server the worker process calls
// over a real socket, which records what arrived and can be told to misbehave.
//
// It runs inside the test binary rather than as a third process on purpose.
// The sink is the test's instrument, not part of the system under test, and a
// Go object with methods is a far better instrument than an HTTP control API
// polled from a shell — `sink.hold()` is exact where "set latency to 6s and
// hope the kill lands inside it" is a guess.
type sink struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	deliveries []delivery
	status     int           // the response to send; 0 means 200
	held       chan struct{} // non-nil while deliveries are being held open
	changed    chan struct{} // closed and replaced whenever anything changes
}

// delivery is one webhook the sink accepted, recorded on arrival — before the
// response is sent, and therefore before the worker can know it succeeded.
// That ordering is what makes a duplicate provable rather than incidental: a
// worker killed between these two moments has delivered the event once and
// recorded nothing.
type delivery struct {
	Event          pgrepo.Event
	EventID        string
	IdempotencyKey string
	Received       time.Time
}

func newSink(t *testing.T) *sink {
	t.Helper()

	s := &sink{t: t, changed: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hook", s.receive)

	s.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		// Release anything still parked in a handler first: Close waits for
		// outstanding requests, and a held delivery would deadlock the
		// cleanup rather than fail the test.
		s.release()
		s.srv.Close()
	})
	return s
}

// url is what the worker is pointed at.
func (s *sink) url() string { return s.srv.URL + "/hook" }

func (s *sink) receive(w http.ResponseWriter, r *http.Request) {
	var event pgrepo.Event
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		http.Error(w, "undecodable event: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.deliveries = append(s.deliveries, delivery{
		Event:          event,
		EventID:        r.Header.Get("X-Event-Id"),
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
		Received:       time.Now(),
	})
	status, held := s.status, s.held
	s.notify()
	s.mu.Unlock()

	if held != nil {
		select {
		case <-held:
		case <-r.Context().Done():
			return
		}
	}

	if status != 0 {
		http.Error(w, "injected failure", status)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// notify wakes every waiter. The caller must hold the lock.
func (s *sink) notify() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// fail makes every subsequent delivery return status. A 5xx is the dependency
// being down and must be retried; a 4xx is our payload being wrong and must
// not be. Which one a test picks is the point of the test.
func (s *sink) fail(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
	s.notify()
}

// heal returns the sink to answering 200.
func (s *sink) heal() {
	s.fail(0)
}

// hold parks every delivery in its handler after recording it, so a test can
// act while a request is genuinely in flight.
func (s *sink) hold() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held == nil {
		s.held = make(chan struct{})
	}
}

// release lets held deliveries answer and stops holding new ones.
func (s *sink) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held != nil {
		close(s.held)
		s.held = nil
	}
}

// received returns every delivery so far, in arrival order.
func (s *sink) received() []delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]delivery(nil), s.deliveries...)
}

// distinct counts the logical events behind the deliveries. At-least-once
// means len(received()) may exceed this; a receiver that deduplicates on
// X-Event-Id sees exactly this many.
func (s *sink) distinct() int {
	ids := map[string]struct{}{}
	for _, d := range s.received() {
		ids[d.EventID] = struct{}{}
	}
	return len(ids)
}

// await blocks until the recorded deliveries satisfy cond, and fails with what
// it did see if they never do.
func (s *sink) await(cond func([]delivery) bool, within time.Duration, what string) []delivery {
	s.t.Helper()

	deadline := time.After(within)
	for {
		s.mu.Lock()
		got := append([]delivery(nil), s.deliveries...)
		changed := s.changed
		s.mu.Unlock()

		if cond(got) {
			return got
		}

		select {
		case <-changed:
		case <-deadline:
			s.t.Fatalf("sink: timed out after %s waiting for %s; received %d: %s",
				within, what, len(got), summarise(got))
		}
	}
}

// awaitCount waits for at least n deliveries.
func (s *sink) awaitCount(n int, within time.Duration) []delivery {
	s.t.Helper()
	return s.await(func(d []delivery) bool { return len(d) >= n }, within,
		plural(n, "delivery", "deliveries"))
}

// awaitEvent waits for one named event about one task.
func (s *sink) awaitEvent(taskID, event string, within time.Duration) delivery {
	s.t.Helper()

	var found delivery
	s.await(func(ds []delivery) bool {
		for _, d := range ds {
			if d.Event.TaskID.String() == taskID && d.Event.Event == event {
				found = d
				return true
			}
		}
		return false
	}, within, event+" for "+taskID)

	return found
}

func summarise(ds []delivery) string {
	if len(ds) == 0 {
		return "none"
	}
	out := ""
	for i, d := range ds {
		if i > 0 {
			out += ", "
		}
		out += d.Event.Event + "/" + d.EventID + " (key " + d.IdempotencyKey + ")"
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
