package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/apperr"
	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/logging"
)

// PoolOptions configures a worker pool.
type PoolOptions struct {
	Store    *Store
	Handlers map[string]HandlerFunc
	// WorkerID identifies this replica in locked_by and attempted_by. It must
	// be unique per process, or the stale-worker guard cannot tell two
	// processes apart.
	WorkerID string
	Config   config.Queue
	Logger   *slog.Logger
	// Notify carries wake-ups from LISTEN/NOTIFY. Nil means poll only.
	Notify <-chan struct{}
}

// Pool claims jobs and runs them.
//
// Archetype: Service. Run blocks, owns every goroutine it starts, and joins
// all of them before returning — nothing escapes it, which is what makes the
// shutdown assertion in the composition root provable rather than hopeful.
type Pool struct {
	store    *Store
	handlers map[string]HandlerFunc
	workerID string
	cfg      config.Queue
	log      *slog.Logger
	backoff  Backoff
	notify   <-chan struct{}

	// inflight is the set of jobs this pool currently holds a lease on. The
	// heartbeat reads it; the workers write it.
	mu       sync.Mutex
	inflight map[int64]context.CancelFunc

	// draining stops the claim loop without cancelling running work.
	draining chan struct{}
	drainOne sync.Once

	// subs receive completion events. Tests wait on these instead of sleeping.
	subMu sync.Mutex
	subs  map[int]chan Event
	subID int
}

// NewPool wires a pool.
func NewPool(o PoolOptions) (*Pool, error) {
	switch {
	case o.Store == nil:
		return nil, errors.New("queue: a store is required")
	case len(o.Handlers) == 0:
		return nil, errors.New("queue: at least one handler is required")
	case o.WorkerID == "":
		return nil, errors.New("queue: a worker id is required")
	}

	log := o.Logger
	if log == nil {
		log = slog.Default()
	}

	return &Pool{
		store:    o.Store,
		handlers: o.Handlers,
		workerID: o.WorkerID,
		cfg:      o.Config,
		log:      log.With(slog.String("component", "queue"), slog.String("worker_id", o.WorkerID)),
		backoff:  NewBackoff(o.Config.Backoff),
		notify:   o.Notify,
		inflight: make(map[int64]context.CancelFunc),
		draining: make(chan struct{}),
		subs:     make(map[int]chan Event),
	}, nil
}

// Run starts the workers and blocks until ctx is cancelled.
func (p *Pool) Run(ctx context.Context) error {
	ctx = logging.Into(ctx, p.log)
	jobs := make(chan *Job)

	var wg sync.WaitGroup
	for i := range p.cfg.Workers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			p.work(ctx, n, jobs)
		}(i)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		p.heartbeat(ctx)
	}()

	p.log.Info("worker pool started", slog.Int("workers", p.cfg.Workers))

	// The claim loop runs on this goroutine, so Run returns only once it has
	// stopped feeding the channel.
	p.claimLoop(ctx, jobs)

	close(jobs)
	wg.Wait()
	p.log.Info("worker pool stopped")
	return nil
}

// Stop drains: stop claiming, let in-flight work finish, then hand back
// anything still running.
//
// Releasing explicitly is what makes a rolling deploy free. A job left to have
// its lease expire is invisible for a full lease period for no reason; only a
// genuine crash should pay that.
func (p *Pool) Stop(ctx context.Context) error {
	p.drainOne.Do(func() { close(p.draining) })

	// Wait for the workers to finish what they already claimed.
	for {
		if p.inflightCount() == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return p.releaseInflight(ctx)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// claimLoop takes batches of jobs and feeds them to the workers.
func (p *Pool) claimLoop(ctx context.Context, jobs chan<- *Job) {
	poll := time.NewTicker(p.cfg.PollInterval.D())
	defer poll.Stop()

	for {
		// Claim until the queue is empty, then wait. Claiming one batch per
		// wake-up would drain a backlog at one batch per poll interval.
		for p.claimBatch(ctx, jobs) {
			if !p.wait(ctx, p.cfg.FetchCooldown.D()) {
				return
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-p.draining:
			return
		case <-poll.C:
			// The safety net. NOTIFY is fire-and-forget and is dropped if
			// nobody is listening, so the ticker is what turns best-effort
			// wake-ups into at-least-once pickup.
		case <-p.notifyChan():
			// The latency optimisation: a job enqueued now is picked up in
			// milliseconds instead of at the next tick.
		}
	}
}

// claimBatch claims once and dispatches. It reports whether there may be more.
func (p *Pool) claimBatch(ctx context.Context, jobs chan<- *Job) bool {
	select {
	case <-ctx.Done():
		return false
	case <-p.draining:
		return false
	default:
	}

	claimed, err := p.store.Claim(ctx, p.workerID, p.cfg.ClaimBatch, p.cfg.Lease.D())
	if err != nil {
		if ctx.Err() == nil {
			p.log.Error("claim failed", slog.Any("err", err))
		}
		return false
	}
	if len(claimed) == 0 {
		return false
	}

	logging.Trace(ctx, "claimed", slog.Int("jobs", len(claimed)), slog.Int("requested", p.cfg.ClaimBatch))

	for _, j := range claimed {
		// Registered before the hand-off, not when the handler starts. A job
		// sitting in the channel already holds a lease, so it has to be
		// heartbeated — and Stop has to wait for it, or a drain can return
		// while a job it just claimed is about to run.
		p.track(j.ID, nil)

		select {
		case jobs <- j:
		case <-ctx.Done():
			p.untrack(j.ID)
			// Hand back what was claimed but never dispatched.
			_, _ = p.store.Release(context.WithoutCancel(ctx), p.workerID, []int64{j.ID})
			return false
		}
	}

	// A full batch means the queue probably has more.
	return len(claimed) == p.cfg.ClaimBatch
}

// work is one worker goroutine.
func (p *Pool) work(ctx context.Context, n int, jobs <-chan *Job) {
	for j := range jobs {
		p.run(ctx, j)
	}
	logging.Trace(ctx, "worker stopped", slog.Int("worker", n))
}

// run executes one job and records what happened.
func (p *Pool) run(ctx context.Context, j *Job) {
	// A per-attempt deadline. A handler with no deadline is precisely how a
	// job gets stuck forever, and the reason the reaper has to exist at all.
	jobCtx, cancel := context.WithTimeout(ctx, p.cfg.JobTimeout.D())
	jobCtx = logging.With(jobCtx,
		slog.Int64("job_id", j.ID),
		slog.String("kind", j.Kind),
		slog.Int("attempt", j.Attempt),
	)

	// Replaces the placeholder registered at dispatch, so the heartbeat can
	// now cancel this job if its lease is lost.
	p.track(j.ID, cancel)
	defer func() {
		p.untrack(j.ID)
		cancel()
	}()

	started := time.Now()
	err := p.invoke(jobCtx, j)
	outcome := p.finish(ctx, j, err, time.Since(started))

	p.publish(Event{JobID: j.ID, Kind: j.Kind, Attempt: j.Attempt, Outcome: outcome, Err: err})
}

// invoke calls the handler, turning a panic into an error.
//
// A panic in one handler must not take the pool down with it: the other
// workers are running unrelated jobs, and the job that panicked deserves the
// same retry treatment as one that returned an error.
func (p *Pool) invoke(ctx context.Context, j *Job) (err error) {
	handler, ok := p.handlers[j.Kind]
	if !ok {
		// No handler will appear by retrying. This is a deploy problem, and
		// the job should stop consuming attempts until it is fixed.
		return Terminal(fmt.Errorf("no handler for kind %q", j.Kind))
	}

	defer func() {
		if r := recover(); r != nil {
			logging.From(ctx).Error("handler panicked", slog.Any("panic", r))
			err = fmt.Errorf("handler panicked: %v", r)
		}
	}()

	return handler(ctx, j)
}

// finish writes the outcome, and reports what it decided.
func (p *Pool) finish(ctx context.Context, j *Job, cause error, took time.Duration) Outcome {
	// Detached: the completion write must land even when the process is
	// shutting down, or the job looks abandoned and is reclaimed for no reason.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	outcome, held, err := p.write(writeCtx, j, cause)
	if err != nil {
		p.log.Error("could not record job outcome",
			slog.Int64("job_id", j.ID), slog.String("outcome", string(outcome)), slog.Any("err", err))
		return OutcomeLost
	}
	if !held {
		// Zero rows affected is not an error. It means the reaper reclaimed
		// this job while it was running and someone else owns it now, and the
		// guard is what stopped this worker from overwriting their result.
		p.log.Warn("claim lost, outcome discarded",
			slog.Int64("job_id", j.ID), slog.Int("attempt", j.Attempt))
		return OutcomeLost
	}

	p.logOutcome(ctx, j, outcome, cause, took)
	return outcome
}

// write applies the outcome the error implies.
func (p *Pool) write(ctx context.Context, j *Job, cause error) (Outcome, bool, error) {
	switch {
	case cause == nil:
		held, err := p.store.Succeed(ctx, p.workerID, j)
		return OutcomeSucceeded, held, err

	case isSnooze(cause):
		var snooze *SnoozeError
		_ = errors.As(cause, &snooze)
		held, err := p.store.Snooze(ctx, p.workerID, j, snooze.For)
		return OutcomeSnoozed, held, err

	case !retryable(cause):
		// Either the handler said so explicitly, or the error's classification
		// says retrying cannot help. Both are poison-job cases, and burning
		// four more attempts on them only hides the bug.
		held, err := p.store.Cancel(ctx, p.workerID, j, cause)
		return OutcomeCancelled, held, err

	case j.Attempt >= p.maxAttempts(j):
		held, err := p.store.Discard(ctx, p.workerID, j, cause)
		return OutcomeDiscarded, held, err

	default:
		held, err := p.store.Retry(ctx, p.workerID, j, p.backoff.For(j.Attempt), cause)
		return OutcomeRetried, held, err
	}
}

// maxAttempts is how many attempts this job actually gets.
//
// Two values could decide it, and both are meaningful: the row carries a
// per-job budget, and the configuration carries a fleet-wide ceiling. The
// lower wins, so a job may ask for fewer attempts than the fleet allows and
// never more — which is what makes queue.max_attempts a knob an operator can
// turn down mid-incident to stop a failing dependency being hammered.
//
// Without this the configuration would silently do nothing, because the column
// default is what the producer gets when it does not specify.
func (p *Pool) maxAttempts(j *Job) int {
	if p.cfg.MaxAttempts > 0 && p.cfg.MaxAttempts < j.MaxAttempts {
		return p.cfg.MaxAttempts
	}
	return j.MaxAttempts
}

// retryable decides whether trying again could help.
//
// The explicit sentinel wins, and otherwise the shared classification decides
// — the same one the HTTP layer maps to status codes. That is deliberate: an
// error that is a client's mistake over HTTP is a poison job in the queue, and
// the two must not disagree about it.
func retryable(err error) bool {
	if IsTerminal(err) {
		return false
	}
	return apperr.Retryable(err)
}

func isSnooze(err error) bool {
	var s *SnoozeError
	return errors.As(err, &s)
}

func (p *Pool) logOutcome(ctx context.Context, j *Job, outcome Outcome, cause error, took time.Duration) {
	attrs := []slog.Attr{
		slog.Int64("job_id", j.ID),
		slog.String("kind", j.Kind),
		slog.Int("attempt", j.Attempt),
		slog.Float64("dur_ms", float64(took.Microseconds())/1000),
	}
	if cause != nil {
		attrs = append(attrs, slog.Any("err", cause))
	}

	switch outcome {
	case OutcomeDiscarded:
		// The dead-letter state: a human has to look at this.
		p.log.LogAttrs(ctx, slog.LevelError, "job discarded", attrs...)
	case OutcomeRetried, OutcomeCancelled:
		p.log.LogAttrs(ctx, slog.LevelWarn, "job "+string(outcome), attrs...)
	default:
		p.log.LogAttrs(ctx, slog.LevelDebug, "job "+string(outcome), attrs...)
	}
}

// heartbeat extends the lease on everything this pool holds.
//
// Every lease/3, in one statement for all of them. Two consecutive misses are
// tolerated before the reaper acts, which is the same reasoning as a lease TTL
// against a keepalive interval in etcd or Raft.
func (p *Pool) heartbeat(ctx context.Context) {
	tick := time.NewTicker(p.cfg.HeartbeatInterval.D())
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			p.beat(ctx)
		}
	}
}

func (p *Pool) beat(ctx context.Context) {
	ids, cancels := p.snapshot()
	if len(ids) == 0 {
		return
	}

	held, err := p.store.Heartbeat(ctx, p.workerID, ids, p.cfg.Lease.D())
	if err != nil {
		if ctx.Err() == nil {
			p.log.Error("heartbeat failed", slog.Any("err", err))
		}
		return
	}
	logging.Trace(ctx, "heartbeat", slog.Int("held", held), slog.Int("inflight", len(ids)))

	if held == len(ids) {
		return
	}

	// Fewer rows than jobs means at least one lease was reclaimed while its
	// handler is still running. Cancelling now stops the worker doing work
	// nobody will accept — the completion write would be refused by the claim
	// guard anyway, so continuing only wastes the dependency's capacity.
	p.log.Warn("lost leases while running",
		slog.Int("held", held), slog.Int("inflight", len(ids)))
	for _, cancel := range cancels {
		cancel()
	}
}

func (p *Pool) track(id int64, cancel context.CancelFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inflight[id] = cancel
}

func (p *Pool) untrack(id int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.inflight, id)
}

func (p *Pool) snapshot() ([]int64, []context.CancelFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()

	ids := make([]int64, 0, len(p.inflight))
	cancels := make([]context.CancelFunc, 0, len(p.inflight))
	for id, cancel := range p.inflight {
		ids = append(ids, id)
		// Nil until the handler starts: a job that is dispatched but not yet
		// running has nothing to cancel.
		if cancel != nil {
			cancels = append(cancels, cancel)
		}
	}
	return ids, cancels
}

func (p *Pool) inflightCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.inflight)
}

// releaseInflight hands back whatever is still running when the drain deadline
// runs out.
func (p *Pool) releaseInflight(ctx context.Context) error {
	ids, cancels := p.snapshot()
	if len(ids) == 0 {
		return nil
	}

	// Release before cancelling, not after. Cancelling first leaves a window
	// in which the dying handler returns and writes its own outcome, and the
	// job finalizes instead of going back to the queue. Releasing first means
	// that write hits the claim guard and is refused, which is exactly what
	// the guard is for.
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	n, err := p.store.Release(releaseCtx, p.workerID, ids)
	for _, cancelJob := range cancels {
		cancelJob()
	}
	if err != nil {
		return fmt.Errorf("queue: release in-flight jobs: %w", err)
	}
	p.log.Warn("drain deadline reached, jobs released",
		slog.Int("released", n), slog.Int("inflight", len(ids)))
	return nil
}

// notifyChan returns the wake-up channel, or nil when there is none. A nil
// channel blocks forever in a select, which is exactly the poll-only
// behaviour.
func (p *Pool) notifyChan() <-chan struct{} { return p.notify }

// wait sleeps unless the pool is stopping.
func (p *Pool) wait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-p.draining:
		return false
	case <-t.C:
		return true
	}
}
