package idempotency

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arasHi87/gogolook-task-api/internal/postgres"
)

// reserveSQL claims a key, or reports what the key already holds, in one
// statement.
//
// One statement rather than "insert, and select on conflict", and the reason
// is a race the two-statement version cannot close: between the failed insert
// and the select, the row can be purged, and the select then finds nothing
// while the insert has already refused to create it.
//
// DO UPDATE rather than DO NOTHING for the same reason — DO NOTHING returns no
// row, so there is nothing to read back. Setting the key to itself changes no
// value, and takes the row lock that serialises two concurrent requests
// holding the same key: the second waits for the first to commit and then sees
// its result, instead of both deciding they are first.
//
// xmax is zero on a tuple this statement inserted and non-zero on one it
// found, which is how the caller tells "reserved" from "already taken".
const reserveSQL = `
INSERT INTO idempotency_keys (key, fingerprint, state, expires_at)
VALUES ($1, $2, 'in_progress', now() + $3::interval)
ON CONFLICT (key) DO UPDATE SET key = idempotency_keys.key
RETURNING (xmax = 0) AS reserved, fingerprint, state,
          coalesce(status_code, 0), coalesce(content_type, ''), coalesce(response_body, ''::bytea)`

const completeSQL = `
UPDATE idempotency_keys
   SET state = 'completed', status_code = $2, content_type = $3, response_body = $4
 WHERE key = $1`

// purgeSQL drops keys past their expiry.
//
// Until it runs, an expired key is still replayed. That is deliberate: the TTL
// is a promise about how long a replay is guaranteed, not a deadline after
// which replaying becomes wrong, and answering a late retry correctly is
// better than executing it twice.
const purgeSQL = `DELETE FROM idempotency_keys WHERE expires_at < now()`

// PostgresStore is the durable store. Keys are shared across every replica,
// which is the only way the guarantee survives a load balancer.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore returns a store backed by pool.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// Reserve implements Store.
func (s *PostgresStore) Reserve(
	ctx context.Context, key string, fingerprint []byte, ttl time.Duration,
) (Unit, *Record, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("idempotency: begin: %w", err)
	}

	var (
		reserved bool
		rec      Record
	)
	err = tx.QueryRow(ctx, reserveSQL, key, fingerprint, ttl.String()).
		Scan(&reserved, &rec.Fingerprint, &rec.State, &rec.StatusCode, &rec.ContentType, &rec.Body)
	if err != nil {
		//nolint:errcheck // the reserve already failed; the rollback's error adds nothing
		_ = tx.Rollback(ctx)
		return nil, nil, fmt.Errorf("idempotency: reserve %q: %w", key, err)
	}

	if !reserved {
		// Nothing was written, so there is nothing to keep. Holding the
		// transaction open would pin a connection for the length of a request
		// that is about to be answered from the row we already have.
		//nolint:errcheck // read-only transaction, nothing to lose
		_ = tx.Rollback(ctx)
		return nil, &rec, nil
	}

	// The request runs inside this transaction: the repository joins it, so
	// the task, its outbox row and the stored response are one commit.
	return &pgUnit{tx: tx, key: key, ctx: postgres.WithTx(ctx, tx)}, nil, nil
}

// Purge implements Store.
//
// Leader-elected, like every other periodic sweep here. The delete itself is
// cheap and idempotent — three replicas running it would mostly delete nothing
// — but they would contend on the same rows for one sweep's worth of work, and
// "the fleet does this once" is easier to reason about than "this happens to
// be harmless when it happens three times".
func (s *PostgresStore) Purge(ctx context.Context) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("idempotency: purge: begin: %w", err)
	}
	//nolint:errcheck // a rollback after a successful commit is a no-op
	defer tx.Rollback(ctx)

	var purged int
	led, err := postgres.WithLeader(ctx, tx, "idempotency-purge", func(ctx context.Context) error {
		tag, err := tx.Exec(ctx, purgeSQL)
		if err != nil {
			return fmt.Errorf("idempotency: purge: %w", err)
		}
		purged = int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return 0, err
	}
	if !led {
		return 0, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("idempotency: purge: commit: %w", err)
	}
	return purged, nil
}

// pgUnit is one reserved key and the open transaction behind it.
type pgUnit struct {
	tx   pgx.Tx
	key  string
	ctx  context.Context
	done bool
}

// Context implements Unit.
func (u *pgUnit) Context() context.Context { return u.ctx }

// Complete implements Unit.
func (u *pgUnit) Complete(status int, contentType string, body []byte) error {
	if u.done {
		return errors.New("idempotency: unit already finished")
	}
	u.done = true

	if _, err := u.tx.Exec(u.ctx, completeSQL, u.key, status, contentType, body); err != nil {
		//nolint:errcheck // the update already failed; the rollback's error adds nothing
		_ = u.tx.Rollback(u.ctx)
		return fmt.Errorf("idempotency: complete %q: %w", u.key, err)
	}
	if err := u.tx.Commit(u.ctx); err != nil {
		return fmt.Errorf("idempotency: commit %q: %w", u.key, err)
	}

	// Only now: the side effects the repository deferred, chiefly the NOTIFY
	// that wakes a worker. Sent before this point it would name a row nobody
	// can see yet.
	postgres.RunAfterCommit(u.ctx)
	return nil
}

// Abandon implements Unit.
func (u *pgUnit) Abandon() {
	if u.done {
		return
	}
	u.done = true

	// A fresh context: the request's may already be cancelled, which is often
	// exactly why we are abandoning, and a rollback on a cancelled context
	// does not run — it leaves the transaction to be cleaned up by the pool
	// on return, which is slower and less obvious.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(u.ctx), 5*time.Second)
	defer cancel()
	//nolint:errcheck // there is no recovery from a failed rollback and no one to tell
	_ = u.tx.Rollback(ctx)
}
