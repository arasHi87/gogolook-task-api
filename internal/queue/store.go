package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the queue's SQL.
//
// Every statement here is a decision. They are kept in one file so the whole
// state machine can be read at once, rather than discovered a query at a time.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a store over pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// claimSQL takes up to n available jobs for one worker.
//
// FOR UPDATE SKIP LOCKED is what makes N workers safe: a row another worker
// has already locked is skipped rather than waited on, so throughput scales
// with workers instead of serialising behind the first one.
//
// The state test is `available` alone, not "available or retryable". A
// retryable job is made available by the scheduler once its backoff has
// elapsed. Conflating the two would let a hot-failing job be re-claimed
// immediately and monopolise the claim query.
//
// ORDER BY inside the CTE, with LIMIT, keeps this an index scan over
// jobs_claim_idx rather than a sort of the whole table.
const claimSQL = `
WITH claimed AS (
    SELECT id
      FROM jobs
     WHERE state = 'available'
       AND scheduled_at <= now()
     ORDER BY priority, scheduled_at, id
     FOR UPDATE SKIP LOCKED
     LIMIT $2
)
UPDATE jobs j
   SET state            = 'running',
       attempt          = j.attempt + 1,
       locked_by        = $1,
       locked_at        = now(),
       lease_expires_at = now() + $3::interval,
       heartbeat_at     = now(),
       attempted_by     = array_append(j.attempted_by, $1)
  FROM claimed c
 WHERE j.id = c.id
RETURNING j.id, j.kind, j.payload, j.attempt, j.max_attempts,
          extract(epoch FROM now() - j.scheduled_at)`

// Claim takes up to batch jobs for workerID and stamps a lease on each.
func (s *Store) Claim(ctx context.Context, workerID string, batch int, lease time.Duration) ([]*Job, error) {
	rows, err := s.pool.Query(ctx, claimSQL, workerID, batch, lease)
	if err != nil {
		return nil, fmt.Errorf("queue: claim: %w", err)
	}
	defer rows.Close()

	jobs := make([]*Job, 0, batch)
	for rows.Next() {
		var (
			j      Job
			waited float64
		)
		if err := rows.Scan(&j.ID, &j.Kind, &j.Payload, &j.Attempt, &j.MaxAttempts, &waited); err != nil {
			return nil, fmt.Errorf("queue: scan claimed job: %w", err)
		}
		// Computed by the database, not by subtracting timestamps here: the
		// worker's clock and the database's are not the same clock, and a
		// queue-wait metric built from two of them measures the skew as often
		// as it measures the wait.
		j.Waited = time.Duration(max(waited, 0) * float64(time.Second))
		jobs = append(jobs, &j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: claim: %w", err)
	}
	return jobs, nil
}

// heartbeatSQL extends the lease on every job a worker currently holds.
//
// One statement for all of them, not one per job: a pool of eight workers
// heartbeating every ten seconds is one round trip, not eight.
//
// The locked_by test is what makes the return count meaningful. A row that has
// been reclaimed no longer names this worker, so it is not updated, and the
// shortfall tells the worker it has lost the job.
const heartbeatSQL = `
UPDATE jobs
   SET heartbeat_at = now(),
       lease_expires_at = now() + $3::interval
 WHERE id = ANY($1)
   AND state = 'running'
   AND locked_by = $2`

// Heartbeat extends the lease on ids and reports how many were still held.
func (s *Store) Heartbeat(ctx context.Context, workerID string, ids []int64, lease time.Duration) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, heartbeatSQL, ids, workerID, lease)
	if err != nil {
		return 0, fmt.Errorf("queue: heartbeat: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// The claim guard, appended to every completion write.
//
// Without it a zombie worker — one that came back from a GC pause or a network
// partition after its lease expired — can mark a job succeeded that the reaper
// already requeued and another worker is currently running. The second
// attempt's result is then silently lost. Asserting the exact claim the work
// was done under makes that impossible: the write matches no row, and zero
// rows affected is the signal that the claim was lost, not an error.
const claimGuard = ` AND state = 'running' AND locked_by = $2 AND attempt = $3`

const succeedSQL = `
UPDATE jobs
   SET state = 'succeeded', finalized_at = now(),
       locked_by = NULL, locked_at = NULL, lease_expires_at = NULL
 WHERE id = $1` + claimGuard

// Succeed finalizes a job. It reports whether the claim still held.
func (s *Store) Succeed(ctx context.Context, workerID string, j *Job) (bool, error) {
	return s.finalize(ctx, succeedSQL, "succeed", workerID, j)
}

// retrySQL puts a failed job back with a backoff and records why.
//
// The error is appended to a history rather than overwriting a last_error
// column. "It failed four times" with only the fourth error, and no record of
// which worker saw which, is not something anyone can act on.
const retrySQL = `
UPDATE jobs
   SET state = 'retryable',
       scheduled_at = now() + $4::interval,
       locked_by = NULL, locked_at = NULL, lease_expires_at = NULL,
       errors = errors || jsonb_build_object(
           'at', now(), 'attempt', attempt, 'worker', $2::text, 'error', $5::text)
 WHERE id = $1` + claimGuard

// Retry reschedules a failed job after the given delay.
func (s *Store) Retry(ctx context.Context, workerID string, j *Job, in time.Duration, cause error) (bool, error) {
	tag, err := s.pool.Exec(ctx, retrySQL, j.ID, workerID, j.Attempt, in, errText(cause))
	if err != nil {
		return false, fmt.Errorf("queue: retry job %d: %w", j.ID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// terminalSQL finalizes a job that will not be tried again. The caller decides
// whether that is cancelled (the failure is not retryable) or discarded (the
// attempts ran out); they are different facts and the dashboards separate them.
const terminalSQL = `
UPDATE jobs
   SET state = $4::job_state, finalized_at = now(),
       locked_by = NULL, locked_at = NULL, lease_expires_at = NULL,
       errors = errors || jsonb_build_object(
           'at', now(), 'attempt', attempt, 'worker', $2::text, 'error', $5::text)
 WHERE id = $1` + claimGuard

// Cancel finalizes a job whose failure retrying cannot fix.
func (s *Store) Cancel(ctx context.Context, workerID string, j *Job, cause error) (bool, error) {
	return s.terminal(ctx, workerID, j, StateCancelled, cause)
}

// Discard finalizes a job that has run out of attempts. This is the
// dead-letter state.
func (s *Store) Discard(ctx context.Context, workerID string, j *Job, cause error) (bool, error) {
	return s.terminal(ctx, workerID, j, StateDiscarded, cause)
}

func (s *Store) terminal(ctx context.Context, workerID string, j *Job, state State, cause error) (bool, error) {
	tag, err := s.pool.Exec(ctx, terminalSQL, j.ID, workerID, j.Attempt, string(state), errText(cause))
	if err != nil {
		return false, fmt.Errorf("queue: %s job %d: %w", state, j.ID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// snoozeSQL reschedules without consuming an attempt.
//
// The attempt decrement is the whole point: the claim incremented it, and a
// snooze is not an attempt. A dependency being down must not spend the job's
// retry budget.
const snoozeSQL = `
UPDATE jobs
   SET state = 'scheduled',
       attempt = attempt - 1,
       scheduled_at = now() + $4::interval,
       locked_by = NULL, locked_at = NULL, lease_expires_at = NULL
 WHERE id = $1` + claimGuard

// Snooze reschedules a job without consuming an attempt.
func (s *Store) Snooze(ctx context.Context, workerID string, j *Job, in time.Duration) (bool, error) {
	tag, err := s.pool.Exec(ctx, snoozeSQL, j.ID, workerID, j.Attempt, in)
	if err != nil {
		return false, fmt.Errorf("queue: snooze job %d: %w", j.ID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// releaseSQL hands a job back untouched.
//
// This is the clean-shutdown path, not the crash path. A rolling deploy that
// left in-flight jobs to expire would make them invisible for a full lease
// period for no reason; releasing them explicitly, with the attempt given
// back, makes a restart cost nothing. Only a genuine crash pays the lease
// timeout.
const releaseSQL = `
UPDATE jobs
   SET state = 'available',
       attempt = attempt - 1,
       locked_by = NULL, locked_at = NULL, lease_expires_at = NULL
 WHERE id = ANY($1)
   AND state = 'running'
   AND locked_by = $2`

// Release returns jobs to the queue without consuming an attempt.
func (s *Store) Release(ctx context.Context, workerID string, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, releaseSQL, ids, workerID)
	if err != nil {
		return 0, fmt.Errorf("queue: release: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func (s *Store) finalize(ctx context.Context, sql, op, workerID string, j *Job) (bool, error) {
	tag, err := s.pool.Exec(ctx, sql, j.ID, workerID, j.Attempt)
	if err != nil {
		return false, fmt.Errorf("queue: %s job %d: %w", op, j.ID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// errText renders a cause for the error history, bounded.
//
// An unbounded error string in a jsonb column that is appended to on every
// attempt is a way to turn a chatty failure into a storage problem.
func errText(err error) string {
	if err == nil {
		return ""
	}
	const max = 1024
	s := err.Error()
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// jsonPayload is a helper for tests and handlers that want the payload typed.
func jsonPayload[T any](j *Job) (T, error) {
	var v T
	if err := json.Unmarshal(j.Payload, &v); err != nil {
		return v, Terminal(fmt.Errorf("queue: payload of job %d is not usable: %w", j.ID, err))
	}
	return v, nil
}

// Unmarshal decodes a job's payload, treating a payload that cannot be decoded
// as terminal: no number of retries will make malformed JSON parse.
func Unmarshal[T any](j *Job) (T, error) { return jsonPayload[T](j) }

var _ = pgx.ErrNoRows

// statsSQL is the one query the metrics collector runs on scrape.
//
// Depth and age together, because neither answers the question alone. Ten
// thousand jobs that drain in twenty seconds is fine; five jobs stuck for an
// hour is an outage, and depth reports the first as the emergency. Age is the
// direct measure of "is the queue actually being served", and it is the signal
// that catches a poison job blocking a partition, every worker wedged on a
// hung dependency, and a deploy that forgot to start the consumer.
//
// Only the states a job can be waiting in. succeeded and discarded rows are
// history, and counting them would make the depth gauge grow forever until the
// purger ran.
const statsSQL = `
SELECT kind,
       state::text,
       count(*),
       coalesce(extract(epoch FROM now() - min(scheduled_at)), 0)
  FROM jobs
 WHERE state IN ('available', 'scheduled', 'running', 'retryable')
 GROUP BY kind, state`

// Stat is one row of the queue's backlog.
type Stat struct {
	Kind  string
	State string
	Count int
	// OldestAge is how long the oldest job in this state has been due. It is
	// negative for scheduled jobs whose time has not come, which is correct
	// and is why the collector clamps rather than the query.
	OldestAge time.Duration
}

// Stats reports the backlog, for the metrics collector.
func (s *Store) Stats(ctx context.Context) ([]Stat, error) {
	rows, err := s.pool.Query(ctx, statsSQL)
	if err != nil {
		return nil, fmt.Errorf("queue: stats: %w", err)
	}
	defer rows.Close()

	var out []Stat
	for rows.Next() {
		var (
			st      Stat
			seconds float64
		)
		if err := rows.Scan(&st.Kind, &st.State, &st.Count, &seconds); err != nil {
			return nil, fmt.Errorf("queue: stats: %w", err)
		}
		st.OldestAge = time.Duration(seconds * float64(time.Second))
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: stats: %w", err)
	}
	return out, nil
}
