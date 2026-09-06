package harness

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

// awaitTimeout bounds every wait in the harness.
//
// One constant rather than an argument at each call site: a scenario should
// say what it is waiting for, not how long it is prepared to wait, and a
// suite where each wait picked its own number is a suite where one of them is
// wrong.
const awaitTimeout = 60 * time.Second

// Webhook is the receiver the worker delivers to: a real HTTP server on a real
// socket, which records what arrived and can be told to misbehave.
//
// It runs inside the test binary rather than as a third process on purpose.
// The receiver is the scenario's instrument, not part of the system under
// test, and a Go object with methods is a far better instrument than an HTTP
// control API polled from a shell — Hold is exact where "set the latency to
// six seconds and hope the kill lands inside it" is a guess.
type Webhook struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	deliveries []Delivery
	status     int           // the response to send; 0 means 200
	held       chan struct{} // non-nil while deliveries are held open
	changed    chan struct{} // closed and replaced whenever anything changes
}

// Delivery is one webhook the receiver accepted, recorded on arrival — before
// the response is sent, and therefore before the worker can know it succeeded.
//
// That ordering is what makes a duplicate provable rather than incidental: a
// worker killed between those two moments has delivered the event once and
// recorded nothing.
type Delivery struct {
	Event          pgrepo.Event
	EventID        string
	IdempotencyKey string
	Received       time.Time
}

func newWebhook(t *testing.T) *Webhook {
	t.Helper()

	w := &Webhook{t: t, changed: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hook", w.receive)

	w.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		// Release anything still parked in a handler first: Close waits for
		// outstanding requests, and a held delivery would deadlock the cleanup
		// rather than fail the scenario.
		w.Release()
		w.srv.Close()
	})
	return w
}

// URL is what the worker is pointed at.
func (w *Webhook) URL() string { return w.srv.URL + "/hook" }

func (w *Webhook) receive(rw http.ResponseWriter, r *http.Request) {
	var event pgrepo.Event
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		http.Error(rw, "undecodable event: "+err.Error(), http.StatusBadRequest)
		return
	}

	w.mu.Lock()
	w.deliveries = append(w.deliveries, Delivery{
		Event:          event,
		EventID:        r.Header.Get("X-Event-Id"),
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
		Received:       time.Now(),
	})
	status, held := w.status, w.held
	w.notify()
	w.mu.Unlock()

	if held != nil {
		select {
		case <-held:
		case <-r.Context().Done():
			return
		}
	}

	if status != 0 {
		http.Error(rw, "injected failure", status)
		return
	}
	rw.WriteHeader(http.StatusOK)
}

// notify wakes every waiter. The caller must hold the lock.
func (w *Webhook) notify() {
	close(w.changed)
	w.changed = make(chan struct{})
}

// Break makes every subsequent delivery return status.
//
// A 5xx is the dependency being down and must be retried; a 4xx is our payload
// being wrong and must not be. Which one a scenario picks is the scenario.
func (w *Webhook) Break(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = status
	w.notify()
}

// Heal returns the receiver to answering 200.
func (w *Webhook) Heal() { w.Break(0) }

// Hold parks every delivery in its handler after recording it, so a scenario
// can act while a request is genuinely in flight.
func (w *Webhook) Hold() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.held == nil {
		w.held = make(chan struct{})
	}
}

// Release lets held deliveries answer and stops holding new ones.
func (w *Webhook) Release() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.held != nil {
		close(w.held)
		w.held = nil
	}
}

// Received returns every delivery so far, in arrival order.
func (w *Webhook) Received() []Delivery {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Delivery(nil), w.deliveries...)
}

// AwaitCount blocks until at least n deliveries have arrived.
func (w *Webhook) AwaitCount(n int) []Delivery {
	w.t.Helper()
	return w.await(func(d []Delivery) bool { return len(d) >= n },
		strconv.Itoa(n)+" deliveries")
}

// AwaitEvent blocks until one named event about one task has arrived.
func (w *Webhook) AwaitEvent(taskID, event string) Delivery {
	w.t.Helper()

	var found Delivery
	w.await(func(ds []Delivery) bool {
		for _, d := range ds {
			if d.Event.TaskID.String() == taskID && d.Event.Event == event {
				found = d
				return true
			}
		}
		return false
	}, event+" for "+taskID)

	return found
}

// Delivered requires an exact number of deliveries.
//
// Exact, because the interesting failures are on both sides: one too few is a
// change that was lost, and one too many is a write that happened twice.
func (w *Webhook) Delivered(want int) {
	w.t.Helper()
	if got := len(w.Received()); got != want {
		w.t.Errorf("%d deliveries, want %d: %s", got, want, summarise(w.Received()))
	}
}

// DeliveredMoreThan requires strictly more than n deliveries, which is how a
// scenario asserts that at-least-once really did deliver twice.
func (w *Webhook) DeliveredMoreThan(n int) {
	w.t.Helper()
	if got := len(w.Received()); got <= n {
		w.t.Errorf("%d deliveries, want more than %d: %s", got, n, summarise(w.Received()))
	}
}

// Distinct requires an exact number of distinct logical events.
//
// At-least-once means the delivery count may exceed this; a receiver that
// deduplicates on X-Event-Id sees exactly this many, and that is the contract.
func (w *Webhook) Distinct(want int) {
	w.t.Helper()

	ids := map[string]struct{}{}
	for _, d := range w.Received() {
		ids[d.EventID] = struct{}{}
	}
	if len(ids) != want {
		w.t.Errorf("%d distinct events, want %d: a change was lost or invented", len(ids), want)
	}
}

// await blocks until the recorded deliveries satisfy cond, and fails with what
// it did see if they never do.
func (w *Webhook) await(cond func([]Delivery) bool, what string) []Delivery {
	w.t.Helper()

	deadline := time.After(awaitTimeout)
	for {
		w.mu.Lock()
		got := append([]Delivery(nil), w.deliveries...)
		changed := w.changed
		w.mu.Unlock()

		if cond(got) {
			return got
		}

		select {
		case <-changed:
		case <-deadline:
			w.t.Fatalf("webhook: timed out after %s waiting for %s; received %d: %s",
				awaitTimeout, what, len(got), summarise(got))
		}
	}
}

func summarise(ds []Delivery) string {
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
