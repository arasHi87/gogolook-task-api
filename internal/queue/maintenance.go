package queue

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// reapSQL reclaims jobs whose lease has lapsed.
//
// This is the crash path. A worker that dies leaves its rows in running with a
// lease that stops being extended; once it expires the job is either retried
// or, if its attempts are spent, dead-lettered.
//
// The heartbeat is what makes a 30-second lease safe for a 10-minute job. A
// static rescue timeout — river's RescueStuckJobsAfter, one hour by default —
// cannot tell a slow job from a dead worker, so it has to be set longer than
// the slowest job anyone might run. A heartbeat is positive proof of liveness,
// so the timeout can be short without ever reclaiming work that is still
// running. This is the one place this queue deliberately goes further than
// river does.
// LEAST applies the same fleet-wide ceiling the pool uses. Without it the
// reaper and the pool would disagree about when a job is out of attempts, and
// a job's fate would depend on whether it failed or its worker died.
const reapSQL = `
UPDATE jobs
   SET state = CASE WHEN attempt >= LEAST(max_attempts, $2) THEN 'discarded'::job_state
                    ELSE 'retryable'::job_state END,
       locked_by = NULL, locked_at = NULL, lease_expires_at = NULL,
       scheduled_at = now() + $1::interval,
       finalized_at = CASE WHEN attempt >= max_attempts THEN now() END,
       errors = errors || jsonb_build_object(
           'at', now(), 'attempt', attempt, 'worker', locked_by,
           'error', 'lease expired: the worker holding this job stopped heartbeating')
 WHERE state = 'running'
   AND lease_expires_at < now()
RETURNING state`

// scheduleSQL makes due jobs claimable.
//
// Separating "is it due" from "is it stuck" is what keeps both queries cheap:
// they are two different questions over two different partial indexes, and
// only one of them is hot.
const scheduleSQL = `
UPDATE jobs
   SET state = 'available'
 WHERE state IN ('retryable', 'scheduled')
   AND scheduled_at <= now()`

// purgeSQL deletes finalized rows past their retention.
//
// A queue table that never deletes grows forever and takes its indexes with
// it, and the claim query slows down in proportion to history it will never
// read. The delete is bounded per sweep so a first run against a large backlog
// does not hold a long transaction.
const purgeSQL = `
DELETE FROM jobs
 WHERE ctid = ANY (ARRAY(
     SELECT ctid FROM jobs
      WHERE finalized_at IS NOT NULL
        AND ((state = 'succeeded' AND finalized_at < now() - $1::interval)
          OR (state IN ('cancelled', 'discarded') AND finalized_at < now() - $2::interval))
      LIMIT $3
 ))`

// purgeBatch bounds one sweep.
const purgeBatch = 10_000

// Reap reclaims jobs whose lease has lapsed, and reports how many went to each
// state.
func (s *Store) Reap(ctx context.Context, backoff Backoff, maxAttempts int) (retryable, discarded int, err error) {
	// One delay for the whole sweep rather than per row: a sweep reclaims jobs
	// that all died together, and computing per-attempt backoff here would
	// mean a correlated subquery for a value the jitter will spread anyway.
	rows, err := s.pool.Query(ctx, reapSQL, backoff.For(1), maxAttempts)
	if err != nil {
		return 0, 0, fmt.Errorf("queue: reap: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			return 0, 0, fmt.Errorf("queue: reap: %w", err)
		}
		if State(state) == StateDiscarded {
			discarded++
			continue
		}
		retryable++
	}
	return retryable, discarded, rows.Err()
}

// Schedule makes due jobs claimable and reports how many.
func (s *Store) Schedule(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, scheduleSQL)
	if err != nil {
		return 0, fmt.Errorf("queue: schedule: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Purge deletes finalized jobs past their retention and reports how many.
func (s *Store) Purge(ctx context.Context, succeeded, discarded time.Duration) (int, error) {
	tag, err := s.pool.Exec(ctx, purgeSQL, succeeded, discarded, purgeBatch)
	if err != nil {
		return 0, fmt.Errorf("queue: purge: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// WithLeader runs fn only if this process wins an advisory lock.
//
// Maintenance is a fleet-wide job, not a per-replica one. Three replicas each
// running `UPDATE jobs ... WHERE lease_expires_at < now()` on the same tick is
// three-way write contention on identical rows, for one sweep's worth of work.
//
// The lock is transaction-scoped, so it is released by the commit rather than
// by a defer that a panic could skip — and a replica that dies mid-sweep
// releases it when its connection drops, rather than blocking maintenance
// until someone notices.
func WithLeader(ctx context.Context, tx pgx.Tx, name string, fn func(context.Context) error) (bool, error) {
	var got bool
	if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1)", lockKey(name)).Scan(&got); err != nil {
		return false, fmt.Errorf("queue: leader lock %q: %w", name, err)
	}
	if !got {
		return false, nil
	}
	return true, fn(ctx)
}

// lockKey turns a name into the bigint the advisory lock functions take.
//
// Advisory locks share one namespace across the whole database, so the key is
// derived from a name rather than picked by hand: two features choosing 1 and
// silently excluding each other is exactly the bug this avoids.
func lockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("taskapi/" + name))
	// Masked to 63 bits rather than reinterpreted, so the key is always
	// positive and the conversion cannot depend on how a signed 64-bit value
	// happens to wrap. Two to the sixty-three keys is not a constraint.
	return int64(h.Sum64() & math.MaxInt64)
}
