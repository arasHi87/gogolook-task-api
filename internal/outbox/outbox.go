// Package outbox writes queue jobs inside a caller's transaction.
//
// This is the transactional outbox pattern, with one simplification: there is
// no broker, so there is no separate outbox table and no relay. Postgres is
// the transport, and the outbox row and the job row are the same row.
//
// What that buys is the elimination — not the mitigation — of the dual-write
// problem. There is no window in which a task exists with no event queued, and
// none in which an event fires for a task that rolled back.
//
// The enforcement mechanism is the signature: Enqueue takes a pgx.Tx and never
// a pool, so it is not possible to enqueue outside a caller's transaction.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Job is one unit of work, written in the caller's transaction.
type Job struct {
	// Kind selects the handler.
	Kind string
	// Payload is marshalled to JSON. It must describe the event completely
	// enough that the handler needs no other source: a job that re-reads the
	// row it was created for sees whatever the row looks like when it runs,
	// not what it looked like when the event happened.
	Payload any
	// UniqueKey deduplicates. While a job holding it is live, a second job
	// with the same key is dropped rather than inserted. Empty means no
	// deduplication.
	UniqueKey string
	// MaxAttempts overrides the table default when non-zero.
	MaxAttempts int
}

// Channel is what a producer notifies after committing, and what the claim
// loop listens on. It is here rather than in the queue package because the
// producer is what sends and the two must agree.
const Channel = "jobs_available"

// Enqueue inserts a job in tx.
//
// It reports whether a row was actually written: a duplicate unique key is not
// an error, it is the deduplication working, and the caller wants to know so
// it can say so in a metric.
func Enqueue(ctx context.Context, tx pgx.Tx, j Job) (inserted bool, err error) {
	if j.Kind == "" {
		return false, fmt.Errorf("outbox: job kind is required")
	}

	payload, err := json.Marshal(j.Payload)
	if err != nil {
		return false, fmt.Errorf("outbox: marshal payload for %s: %w", j.Kind, err)
	}

	// ON CONFLICT DO NOTHING against the partial unique index: the key is
	// unique only while a job holding it is live, so the same logical event
	// can be enqueued again once the previous one has finalized.
	const q = `
		INSERT INTO jobs (kind, payload, unique_key, max_attempts)
		VALUES ($1, $2, NULLIF($3, ''), COALESCE(NULLIF($4, 0), 5))
		ON CONFLICT DO NOTHING
		RETURNING id`

	var id int64
	err = tx.QueryRow(ctx, q, j.Kind, payload, j.UniqueKey, j.MaxAttempts).Scan(&id)
	switch {
	case err == nil:
		return true, nil
	case isNoRows(err):
		return false, nil
	default:
		return false, fmt.Errorf("outbox: enqueue %s: %w", j.Kind, err)
	}
}

// Notify wakes the workers.
//
// It must run after the producing transaction commits, not inside it: a
// notification sent inside a transaction is delivered at commit anyway, but
// one sent by a transaction that then rolls back is never sent at all — and a
// worker woken before the row is visible finds nothing and goes back to sleep.
//
// Delivery is best-effort by design. NOTIFY is fire-and-forget and is dropped
// if nobody is listening, which is why the claim loop also polls: the notify
// is a latency optimisation, the poll is the guarantee.
func Notify(ctx context.Context, q Querier, kind string) error {
	if _, err := q.Exec(ctx, "SELECT pg_notify($1, $2)", Channel, kind); err != nil {
		return fmt.Errorf("outbox: notify: %w", err)
	}
	return nil
}

// Querier is the subset of a pool or connection that Notify needs. It is
// declared here, where it is used, with the minimal method set.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// isNoRows reports whether a query returned nothing, which for the insert
// above means the unique key was already taken.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
