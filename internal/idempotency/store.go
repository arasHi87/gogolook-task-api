package idempotency

import (
	"context"
	"time"
)

// State is where a key is in its lifecycle. The values match the CHECK
// constraint on the column.
const (
	// StateInProgress means a request holding this key is still running.
	StateInProgress = "in_progress"
	// StateCompleted means the response is stored and can be replayed.
	StateCompleted = "completed"
)

// Record is what a key already knows about the request that claimed it.
type Record struct {
	Fingerprint []byte
	State       string
	StatusCode  int
	ContentType string
	Body        []byte
}

// Completed reports whether there is a response to replay.
func (r *Record) Completed() bool { return r.State == StateCompleted }

// Store remembers which keys have been used and what they produced.
type Store interface {
	// Reserve claims key for this request.
	//
	// Exactly one of the two results is non-nil. A Unit means the key is ours
	// and the request should run inside it. A Record means someone got there
	// first, and the caller decides between replaying it, refusing it as key
	// reuse, or refusing it as still in flight.
	Reserve(ctx context.Context, key string, fingerprint []byte, ttl time.Duration) (Unit, *Record, error)

	// Purge deletes expired keys and returns how many went.
	Purge(ctx context.Context) (int, error)
}

// Unit is one reserved key, and the transaction the request runs in.
//
// Exactly one of Complete and Abandon must be called. Abandon after Complete
// is a no-op, so `defer u.Abandon()` is the safe way to hold one.
type Unit interface {
	// Context returns the context the request must run under. It carries
	// whatever the store needs the repository to join — for Postgres, the open
	// transaction — so that the write and the stored response commit together.
	Context() context.Context

	// Complete stores the response and commits everything the request wrote.
	//
	// The content type travels with the body because a body replayed without
	// the header that says how to read it forces the replay path to assume,
	// and an assumption that is true today is a bug the day it stops being.
	Complete(status int, contentType string, body []byte) error

	// Abandon discards the reservation and everything the request wrote, so a
	// retry starts clean. A request that failed must not leave a key behind:
	// the client would then be refused a retry of something that never
	// happened.
	Abandon()
}
