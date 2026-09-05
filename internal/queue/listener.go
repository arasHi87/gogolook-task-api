package queue

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/arasHi87/gogolook-task-api/internal/logging"
	"github.com/arasHi87/gogolook-task-api/internal/outbox"
)

// Listener turns Postgres notifications into wake-ups for the claim loop.
//
// Polling alone at a two-second interval costs two seconds of median latency
// for no reason. NOTIFY alone is unreliable: notifications are fire-and-forget,
// dropped if nobody is listening, and lost across a reconnect. So both — the
// notify is the latency optimisation and the poll is the guarantee.
//
// Archetype: Service.
type Listener struct {
	dsn    string
	log    *slog.Logger
	wakeUp chan struct{}
}

// NewListener returns a listener on the outbox channel.
//
// It takes a DSN rather than the pool because LISTEN occupies a connection for
// its whole life, and a pooled connection cannot safely be held that way: the
// pool would either lose a connection permanently or hand it to someone else
// mid-listen.
func NewListener(dsn string, log *slog.Logger) *Listener {
	if log == nil {
		log = slog.Default()
	}
	return &Listener{
		dsn: dsn,
		log: log.With(slog.String("component", "queue-listener")),
		// Buffered by one, and coalescing: a thousand notifications and one
		// notification mean the same thing to a claim loop that is about to
		// drain the queue anyway.
		wakeUp: make(chan struct{}, 1),
	}
}

// Notifications is the channel the claim loop selects on.
func (l *Listener) Notifications() <-chan struct{} { return l.wakeUp }

// Run listens until ctx is cancelled, reconnecting as needed.
func (l *Listener) Run(ctx context.Context) error {
	ctx = logging.Into(ctx, l.log)

	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		err := l.listen(ctx)

		// Checked before the error, because a cancelled context is why listen
		// returned: shutting down is not a failure to report or retry. The
		// context error is returned rather than nil so the reason travels with
		// it; the worker table treats a cancellation as a clean stop.
		if ctx.Err() != nil {
			l.log.Info("listener stopped")
			return ctx.Err()
		}

		l.log.Warn("listener disconnected, reconnecting",
			slog.Duration("in", backoff), slog.Any("err", err))

		// Anything notified while disconnected was lost, so the claim loop is
		// woken on reconnect. The poll would find it eventually, but there is
		// no reason to wait for the tick.
		l.wake()

		if !sleep(ctx, backoff) {
			return ctx.Err()
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// listen holds one connection until it fails.
func (l *Listener) listen(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, l.dsn)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{outbox.Channel}.Sanitize()); err != nil {
		return err
	}
	l.log.Info("listening for job notifications", slog.String("channel", outbox.Channel))

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		logging.Trace(ctx, "notification", slog.String("payload", n.Payload))
		l.wake()
	}
}

// wake nudges the claim loop without blocking. A full buffer already means
// "there is work", so a second signal adds nothing.
func (l *Listener) wake() {
	select {
	case l.wakeUp <- struct{}{}:
	default:
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
