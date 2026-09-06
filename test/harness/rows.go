package harness

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// This file is how a scenario sees the write side: the rows the processes
// actually left behind.
//
// The queue's own tests wait on an in-process event channel and never sleep.
// From out here there is no such channel — the events belong to the worker's
// address space — so the two observable signals are the webhook arriving,
// which Webhook delivers without polling, and the row's final state, which is
// polled below. Every poll is bounded and reports what it last saw, so a
// timeout says which state the job was stuck in rather than just that time
// ran out.

// pollInterval is how often a row assertion re-reads. Short enough not to
// dominate a scenario, long enough not to hammer the database while a worker
// is trying to use it.
const pollInterval = 50 * time.Millisecond

// Jobs is the queue's bookkeeping.
type Jobs struct {
	t    *testing.T
	pool *pgxpool.Pool
}

// Job is one row of it.
type Job struct {
	ID          int64
	Kind        string
	State       string
	Attempt     int
	MaxAttempts int
	LockedBy    *string
	UniqueKey   *string
	AttemptedBy []string
	Errors      []map[string]any
}

// JobSet is the result of a query, with the assertions a scenario makes about
// a whole set rather than one row.
type JobSet struct {
	t    *testing.T
	Rows []Job
}

// All reads every job, oldest first.
func (j *Jobs) All() JobSet {
	j.t.Helper()

	const q = `SELECT id, kind, state::text, attempt, max_attempts, locked_by, unique_key,
	                  attempted_by, errors
	           FROM jobs ORDER BY id`

	rows, err := j.pool.Query(j.t.Context(), q)
	if err != nil {
		j.t.Fatalf("read jobs: %v", err)
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		var (
			job  Job
			errs []byte
		)
		if err := rows.Scan(&job.ID, &job.Kind, &job.State, &job.Attempt, &job.MaxAttempts,
			&job.LockedBy, &job.UniqueKey, &job.AttemptedBy, &errs); err != nil {
			j.t.Fatalf("scan job: %v", err)
		}
		if err := json.Unmarshal(errs, &job.Errors); err != nil {
			j.t.Fatalf("decode errors of job %d: %v", job.ID, err)
		}
		out = append(out, job)
	}
	if err := rows.Err(); err != nil {
		j.t.Fatalf("read jobs: %v", err)
	}
	return JobSet{t: j.t, Rows: out}
}

// AwaitSettled waits until n jobs exist and none of them is still in flight.
//
// "Not in flight" rather than "all succeeded" on purpose: a scenario that
// wants every job to succeed says so with AllSucceeded, and one about failure
// gets a useful message instead of a timeout when a job lands in discarded.
func (j *Jobs) AwaitSettled(n int) JobSet {
	j.t.Helper()

	return j.await(func(js []Job) bool {
		if len(js) < n {
			return false
		}
		for _, job := range js {
			switch job.State {
			case "succeeded", "cancelled", "discarded":
			default:
				return false
			}
		}
		return true
	}, fmt.Sprintf("%d settled jobs", n))
}

// AwaitAttempt waits until some job has reached at least the given attempt,
// which is how a scenario waits for a retry without sleeping through one.
func (j *Jobs) AwaitAttempt(n int) JobSet {
	j.t.Helper()

	return j.await(func(js []Job) bool {
		for _, job := range js {
			if job.Attempt >= n {
				return true
			}
		}
		return false
	}, fmt.Sprintf("a job on attempt %d", n))
}

// Await polls until the rows satisfy cond.
func (j *Jobs) await(cond func([]Job) bool, what string) JobSet {
	j.t.Helper()

	deadline := time.Now().Add(awaitTimeout)
	for {
		got := j.All()
		if cond(got.Rows) {
			return got
		}
		if time.Now().After(deadline) {
			j.t.Fatalf("timed out after %s waiting for %s; jobs are %s",
				awaitTimeout, what, describe(got.Rows))
		}
		time.Sleep(pollInterval)
	}
}

// AllSucceeded requires every job in the set to have finished successfully.
func (s JobSet) AllSucceeded() JobSet {
	s.t.Helper()
	for _, j := range s.Rows {
		if j.State != "succeeded" {
			s.t.Errorf("job %d (%s) is %s, want succeeded: errors=%v", j.ID, j.Kind, j.State, j.Errors)
		}
	}
	return s
}

// AllInState requires every job to be in one state, which is how a scenario
// asserts what a crash left behind.
func (s JobSet) AllInState(want string) JobSet {
	s.t.Helper()
	for _, j := range s.Rows {
		if j.State != want {
			s.t.Errorf("job %d is %s, want %s", j.ID, j.State, want)
		}
	}
	return s
}

// NoneInState requires no job to be in a state.
func (s JobSet) NoneInState(unwanted string) JobSet {
	s.t.Helper()
	for _, j := range s.Rows {
		if j.State == unwanted {
			s.t.Errorf("job %d is %s and should not be: errors=%v", j.ID, j.State, j.Errors)
		}
	}
	return s
}

// Count requires an exact number of rows.
func (s JobSet) Count(want int) JobSet {
	s.t.Helper()
	if len(s.Rows) != want {
		s.t.Fatalf("%d jobs, want %d: %s", len(s.Rows), want, describe(s.Rows))
	}
	return s
}

// AllClaimed requires every job to still be held by a worker, which after a
// crash is the point: the rows say running and nothing is running them.
func (s JobSet) AllClaimed() JobSet {
	s.t.Helper()
	for _, j := range s.Rows {
		if j.LockedBy == nil {
			s.t.Errorf("job %d is not claimed by anyone", j.ID)
		}
	}
	return s
}

// EachRetried requires every job to have been attempted more than once by more
// than one worker — the record a reclaimed lease leaves behind.
func (s JobSet) EachRetried() JobSet {
	s.t.Helper()
	for _, j := range s.Rows {
		if j.Attempt < 2 {
			s.t.Errorf("job %d succeeded on attempt %d; it should have been retried", j.ID, j.Attempt)
		}
		if len(j.AttemptedBy) < 2 {
			s.t.Errorf("job %d records %d workers; the crashed one and its replacement are two",
				j.ID, len(j.AttemptedBy))
		}
	}
	return s
}

// AttemptsBelow requires no job to have spent more than n attempts, which is
// how a scenario proves a snooze did not burn a retry budget.
func (s JobSet) AttemptsBelow(n int) JobSet {
	s.t.Helper()
	for _, j := range s.Rows {
		if j.Attempt >= n {
			s.t.Errorf("job %d is on attempt %d of %d; its budget was spent on someone else's failure",
				j.ID, j.Attempt, n)
		}
	}
	return s
}

// One returns the only job, failing if there is not exactly one.
func (s JobSet) One() Job {
	s.t.Helper()
	s.Count(1)
	return s.Rows[0]
}

// ErrorsMention requires the recorded error history to contain a substring.
// "It failed and then worked" with no idea why is not an answer anyone can act
// on, which is why the history is kept at all.
func (s JobSet) ErrorsMention(want string) JobSet {
	s.t.Helper()

	encoded, _ := json.Marshal(s.Rows[0].Errors)
	if len(s.Rows[0].Errors) == 0 {
		s.t.Errorf("job %d kept no record of the failures it survived", s.Rows[0].ID)
		return s
	}
	if !strings.Contains(string(encoded), want) {
		s.t.Errorf("the recorded errors do not mention %q: %s", want, encoded)
	}
	return s
}

// UniqueKeys returns the set of dedup keys, so a scenario can assert that one
// write produced one identified event.
func (s JobSet) UniqueKeys() map[string]struct{} {
	s.t.Helper()

	keys := map[string]struct{}{}
	for _, j := range s.Rows {
		if j.UniqueKey == nil {
			s.t.Errorf("job %d has no unique key, so a retried write would enqueue it twice", j.ID)
			continue
		}
		keys[*j.UniqueKey] = struct{}{}
	}
	return keys
}

// Rows is a table a scenario counts.
type Rows struct {
	t     *testing.T
	pool  *pgxpool.Pool
	table string
}

// Count requires an exact number of rows.
func (r *Rows) Count(want int) {
	r.t.Helper()

	var n int
	q := "SELECT count(*) FROM " + r.table
	if err := r.pool.QueryRow(r.t.Context(), q).Scan(&n); err != nil {
		r.t.Fatalf("%s: %v", q, err)
	}
	if n != want {
		r.t.Errorf("%d rows in %s, want %d", n, r.table, want)
	}
}

// describe renders the job table for a failure message.
func describe(js []Job) string {
	if len(js) == 0 {
		return "none"
	}

	counts := map[string]int{}
	for _, j := range js {
		counts[j.State]++
	}
	keys := make([]string, 0, len(counts))
	for state := range counts {
		keys = append(keys, state)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, state := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", state, counts[state]))
	}
	return strings.Join(parts, " ")
}
