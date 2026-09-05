package e2e

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// This file is how the harness sees the write side: the rows the processes
// actually left behind.
//
// The queue's own tests wait on an in-process event channel and never sleep.
// From out here there is no such channel — the events belong to the worker's
// address space — so the two observable signals are the webhook arriving,
// which the sink delivers without polling, and the row's final state, which is
// polled below. Every poll is bounded by a named deadline and reports what it
// last saw, so a timeout says which state the job was stuck in rather than
// just that time ran out.

// job is one row of the queue's bookkeeping.
type job struct {
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

// jobs reads every job, oldest first.
func (h *harness) jobs() []job {
	h.t.Helper()

	const q = `SELECT id, kind, state::text, attempt, max_attempts, locked_by, unique_key,
	                  attempted_by, errors
	           FROM jobs ORDER BY id`

	rows, err := h.DB.Query(h.t.Context(), q)
	if err != nil {
		h.t.Fatalf("read jobs: %v", err)
	}
	defer rows.Close()

	var out []job
	for rows.Next() {
		var (
			j    job
			errs []byte
		)
		if err := rows.Scan(&j.ID, &j.Kind, &j.State, &j.Attempt, &j.MaxAttempts,
			&j.LockedBy, &j.UniqueKey, &j.AttemptedBy, &errs); err != nil {
			h.t.Fatalf("scan job: %v", err)
		}
		if err := json.Unmarshal(errs, &j.Errors); err != nil {
			h.t.Fatalf("decode errors of job %d: %v", j.ID, err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatalf("read jobs: %v", err)
	}
	return out
}

// awaitJobs polls until the job rows satisfy cond.
func (h *harness) awaitJobs(cond func([]job) bool, within time.Duration, what string) []job {
	h.t.Helper()

	deadline := time.Now().Add(within)
	for {
		got := h.jobs()
		if cond(got) {
			return got
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out after %s waiting for %s; jobs are %s", within, what, describe(got))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// awaitSettled waits until n jobs exist and none of them is still in flight.
//
// "Not in flight" rather than "all succeeded" on purpose: a test that wants
// every job to succeed says so, and a test about failure gets a useful message
// instead of a timeout when a job lands in discarded.
func (h *harness) awaitSettled(n int, within time.Duration) []job {
	h.t.Helper()

	return h.awaitJobs(func(js []job) bool {
		if len(js) < n {
			return false
		}
		for _, j := range js {
			switch j.State {
			case "succeeded", "cancelled", "discarded":
			default:
				return false
			}
		}
		return true
	}, within, fmt.Sprintf("%d settled jobs", n))
}

// requireAllSucceeded fails unless every job finished successfully.
func (h *harness) requireAllSucceeded(js []job) {
	h.t.Helper()

	for _, j := range js {
		if j.State != "succeeded" {
			h.t.Errorf("job %d (%s) is %s, want succeeded: errors=%v", j.ID, j.Kind, j.State, j.Errors)
		}
	}
}

// keyRows is how many idempotency keys are stored.
func (h *harness) keyRows() int {
	h.t.Helper()

	var n int
	if err := h.DB.QueryRow(h.t.Context(), `SELECT count(*) FROM idempotency_keys`).Scan(&n); err != nil {
		h.t.Fatalf("count idempotency keys: %v", err)
	}
	return n
}

// taskCount is how many rows survive in the domain table.
func (h *harness) taskCount() int {
	h.t.Helper()

	var n int
	if err := h.DB.QueryRow(h.t.Context(), `SELECT count(*) FROM tasks`).Scan(&n); err != nil {
		h.t.Fatalf("count tasks: %v", err)
	}
	return n
}

// describe renders the job table for a failure message.
func describe(js []job) string {
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
