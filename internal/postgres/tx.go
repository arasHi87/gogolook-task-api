package postgres

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"
)

// This file is the unit of work: a transaction opened by one layer and joined
// by another, carried in the context.
//
// It exists for exactly one requirement, and it is worth stating plainly
// because ambient transactions are otherwise a bad idea. API-level idempotency
// has to record "this key produced this response" in the same transaction as
// the write that produced it. Two transactions cannot do it: whichever commits
// second can fail, leaving either a key that promises a response for a task
// that does not exist, or a task whose key is stuck half-written and whose
// retry is refused until the TTL expires.
//
// The transaction is opened by the middleware, joined by the repository, and
// committed by the middleware. Nothing else joins, and a repository with no
// ambient transaction behaves exactly as it did before.

// txKey addresses the ambient transaction. An unexported struct type, so no
// other package can collide with it or reach the value without going through
// TxFrom.
type txKey struct{}

// unitOfWork is what the context actually carries: the transaction, and the
// side effects that must not happen until it commits.
type unitOfWork struct {
	tx pgx.Tx

	mu    sync.Mutex
	after []func(context.Context)
}

// WithTx returns a context carrying tx, so a repository called under it joins
// the caller's transaction instead of opening its own.
func WithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txKey{}, &unitOfWork{tx: tx})
}

// TxFrom returns the ambient transaction, if there is one.
func TxFrom(ctx context.Context) (pgx.Tx, bool) {
	u, ok := ctx.Value(txKey{}).(*unitOfWork)
	if !ok {
		return nil, false
	}
	return u.tx, true
}

// AfterCommit registers a side effect to run once the ambient transaction has
// committed, and reports whether there was one to register it with.
//
// The caller that gets false owns its own transaction and should just do the
// work itself. The caller that gets true must not: a NOTIFY sent before the
// commit points at a row nobody can see yet, and a worker woken by it finds
// nothing and goes back to sleep — turning a wake-up into a poll interval of
// latency, which is the opposite of what the notification is for.
func AfterCommit(ctx context.Context, fn func(context.Context)) bool {
	u, ok := ctx.Value(txKey{}).(*unitOfWork)
	if !ok {
		return false
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	u.after = append(u.after, fn)
	return true
}

// RunAfterCommit runs everything AfterCommit registered, in order. Only the
// owner of the transaction calls it, and only after its commit succeeded.
func RunAfterCommit(ctx context.Context) {
	u, ok := ctx.Value(txKey{}).(*unitOfWork)
	if !ok {
		return
	}

	u.mu.Lock()
	hooks := u.after
	u.after = nil
	u.mu.Unlock()

	for _, fn := range hooks {
		fn(ctx)
	}
}
