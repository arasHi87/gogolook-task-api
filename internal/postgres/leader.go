package postgres

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"

	"github.com/jackc/pgx/v5"
)

// WithLeader runs fn only if this process wins an advisory lock.
//
// Periodic maintenance is a fleet-wide job, not a per-replica one. Three
// replicas each running the same sweep on the same tick is three-way write
// contention on identical rows, for one sweep's worth of work.
//
// The lock is transaction-scoped, so it is released by the commit rather than
// by a defer that a panic could skip — and a replica that dies mid-sweep
// releases it when its connection drops, rather than blocking maintenance
// until someone notices.
func WithLeader(ctx context.Context, tx pgx.Tx, name string, fn func(context.Context) error) (bool, error) {
	var got bool
	if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1)", lockKey(name)).Scan(&got); err != nil {
		return false, fmt.Errorf("postgres: leader lock %q: %w", name, err)
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
