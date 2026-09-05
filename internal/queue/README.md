# The queue

A job queue in the `jobs` table, which is also the transactional outbox.

## Why this is here rather than `riverqueue/river`

In production I would use river. It is the mature Go/Postgres queue and it
solves this exact problem. It is not used here because the mechanism —
`FOR UPDATE SKIP LOCKED`, a lease with heartbeats, the stale-worker guard, the
transactional outbox — is what this exercise is testing, and a `river.Config{}`
literal is not something anyone can be asked to defend line by line.

Rediscovering river's lessons by accident would be a different mistake. What
follows is what was taken from it deliberately, what was declined, and the one
place this goes further.

## Adopted

| Mechanism | The failure it prevents |
|---|---|
| `retryable` distinct from `available` | A failed job that goes straight back to `available` is instantly re-claimable, hot-loops the pool and starves fresh work. The scheduler is what moves it back once its backoff has elapsed. |
| A separate scheduler loop | "Is it due" and "is it stuck" are different questions over different partial indexes, and only one of them is hot. |
| Terminal errors (`queue.Terminal`) | Retrying a poison job — a malformed payload, a webhook returning 400 — five times is pure waste and hides the real bug behind a queue that merely looks slow. This is the single most valuable thing a handler can say. |
| Snooze, without consuming an attempt | Backpressure is not failure. A circuit breaker being open means the dependency is down, not the job, and burning the job's budget on someone else's outage dead-letters work that was never wrong. |
| Panic containment per job | A panic in one handler must not take the pool down, and the job that panicked deserves the same retry treatment as one that returned an error. |
| Leader election for maintenance | N replicas running the same `UPDATE ... WHERE lease_expires_at < now()` is N-way write contention on identical rows for one sweep's worth of work. |
| `errors` history and `attempted_by` | "It failed four times" with only the fourth error, and no record of which worker saw which, is not something anyone can act on. |
| `attempt^4` backoff with jitter | `2^n` is too aggressive early and too short at the tail. Without jitter, every replica retries in lockstep and a recovering dependency goes straight back down. |
| Per-attempt job timeout | A handler with no deadline is precisely how a job gets stuck forever, and the reason the reaper has to exist. |
| A completion event channel | Integration tests that sleep for "long enough" become the flaky ones everybody skips. Subscribe, enqueue, wait with a deadline. The biggest single testing win here. |
| Fetch cooldown | A thousand-row insert burst should not become a thousand claim round-trips. |
| Unique jobs by key | Duplicate events. The index is partial over the live states, so a key is reserved only while a job holding it is live. |

## Declined

| Feature | Why not here |
|---|---|
| Per-queue `MaxWorkers` | There is one job kind. It is the right answer for isolating slow kinds from fast ones, and it is the scale answer to cite. |
| Periodic jobs | The maintenance loops are plain tickers in the composition root's worker table. Cron-in-queue is a different problem. |
| Middleware and hooks | One handler. A function wrapper is enough; a plugin system for a single caller is architecture theatre. |
| Reindexer | Index bloat becomes real at sustained high volume. Noted in the scale paragraph rather than built. |
| A UI | Grafana is the console. |
| Client fleet tracking | One to three replicas. |

## Where this goes further

**Lease plus heartbeat.** River rescues stuck jobs on a static
`RescueStuckJobsAfter`, one hour by default, which cannot distinguish a slow
job from a dead worker — so it has to be set longer than the slowest job anyone
might run, and a crashed worker's jobs are invisible for that whole window.

A heartbeat is positive proof of liveness. Each worker extends the lease on
everything it holds every `lease/3`, in one batched statement, so two
consecutive misses are tolerated before the reaper acts — the same reasoning as
a lease TTL against a keepalive interval in etcd or Raft. That makes a
30-second lease safe for a 10-minute job: crashed work comes back in seconds,
and slow work is never taken away.

The cost is the stale-worker guard. Every completion write asserts the exact
claim it was made under:

```sql
WHERE id = $1 AND state = 'running' AND locked_by = $2 AND attempt = $3
```

Without it, a worker that returns from a GC pause after its lease expired can
mark a job succeeded that the reaper already requeued and someone else is
running — silently losing the second attempt's result. Zero rows affected is
not an error here; it is the signal that the claim was lost.

## Honest limits

- **At-least-once, not exactly-once.** Exactly-once *effects* are achievable
  and the handlers are written for it. Exactly-once *delivery* is not, and the
  webhook handler says so by sending `Idempotency-Key` and `X-Event-Id` and
  documenting that the receiver must deduplicate.
- **Maintenance is required.** A failed job goes to `retryable` and stays there
  until a scheduler moves it back. A fleet where nothing runs maintenance stops
  retrying, silently. `TestRetriesNeedTheScheduler` pins that down.
- **Throughput ceiling.** Comfortable into the low thousands of jobs a second.
  Past roughly 10k/s you want a real broker: `LISTEN/NOTIFY` serialises on a
  global lock, and the state churn makes the table a vacuum problem before
  that.
