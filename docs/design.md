# Design

Everything here is beyond what the exercise asks for, and each piece is here
because of a specific failure it prevents. The README is the short version; this
is the argument.

- [The contract comes from one source](#the-contract-comes-from-one-source)
- [Storage, and the transactional outbox](#storage-and-the-transactional-outbox)
- [The queue](#the-queue)
- [Idempotency, at three layers](#idempotency-at-three-layers)
- [Rate limiting and circuit breaking](#rate-limiting-and-circuit-breaking)
- [Observability](#observability)
- [Operability](#operability)

## The contract comes from one source

`proto/task/v1/task.proto` declares the messages, the validation rules and the
HTTP mapping. From it, [buf](../buf.gen.yaml) generates the Go types, the Connect
handler and `docs/openapi.yaml`; at runtime, [vanguard](../internal/runtime/handler.go)
reads the *same* `google.api.http` annotations and transcodes REST onto that
handler.

So the served routes and the published document cannot drift, because they are
the same annotations. CI regenerates and fails on any diff.

One consequence worth naming: `status` is an `int32` with a
`buf.validate` rule rather than a proto enum. An enum would serialise as
`"STATUS_COMPLETED"` in JSON and the exercise asks for an integer.

**Two surfaces.** `/api/v1/tasks` is canonical; `/tasks` is what the exercise
specifies and is served identically, carrying `Deprecation`, `Sunset` and `Link`
headers. A 308 redirect was rejected: `curl` without `-L` reports the 308, and a
reviewer working from a checklist reads that as broken.

## Storage, and the transactional outbox

`--storage.backend=postgres` swaps the in-memory repository for a real one. The
interesting part is not the SQL, it is that **the task row and its event are one
transaction**:

```sql
BEGIN;
  INSERT INTO tasks   ...;
  INSERT INTO jobs    ...;   -- the outbox row, keyed by the change
COMMIT;
                             -- and only now, pg_notify
```

There is no broker, so the outbox table *is* the queue table. Either the task and
its event both exist or neither does. The alternative — write the row, then
publish — has a window in which the process dies between the two and the event is
lost with no trace.

The notification is deliberately outside the transaction. Sent inside, it would
still be delivered at commit, but a worker woken by a transaction that then
rolled back finds nothing.

## The queue

A Postgres-backed job queue: `FOR UPDATE SKIP LOCKED` claiming, leases with
heartbeats, a reaper, a scheduler, leader-elected maintenance, and
`LISTEN/NOTIFY` with polling as the safety net.

It is written rather than imported, and [`internal/queue/README.md`](../internal/queue/README.md)
is a census of it against [river](https://github.com/riverqueue/river): twelve
mechanisms taken deliberately, each with the failure it prevents, and six
declined with the reason. Reading the leading implementation and being able to
say which of its decisions you took is a different claim from having used it.

The state machine, and the one edge that surprises people:

```
available ──claim──► running ──ok──────────► succeeded
     ▲                  │
     │                  ├──retryable────────► retryable ──(scheduled_at)──┐
     │                  ├──terminal─────────► cancelled                   │
     │                  ├──snooze───────────► scheduled ─────────────────►│
     │                  └──attempts spent───► discarded                   │
     └──────────────────── scheduler ────────────────────────────────────-┘
```

`retryable` is deliberately distinct from `available`. A failed job that went
straight back to `available` would be instantly re-claimable, hot-loop the pool
and starve fresh work; the scheduler is what moves it back once its backoff has
elapsed.

**See it survive a crash:**

```bash
task up && task demo:crash
```

That kills the worker with `SIGKILL` mid-delivery — no drain, no chance to
release anything — and shows the reaper reclaiming the expired leases and another
worker finishing the work. It also shows the webhook being delivered **twice**,
which is the correct answer rather than a tolerated flaw; see [limits.md](limits.md).

## Idempotency, at three layers

1. **`Idempotency-Key` on writes** — per the IETF draft. Same key and same
   request replays the stored response byte for byte; same key and a *different*
   request is 422; same key while the first is still running is 409. A failed
   request releases its key, so the client can retry something that never
   happened.
2. **Enqueue dedup** — a partial unique index over the live states, so the same
   logical change cannot be queued twice while one is in flight.
3. **Delivery** — the receiver is told enough to deduplicate (`X-Event-Id`,
   `Idempotency-Key`) and the contract says it must, because there is no
   transaction spanning our database and someone else's HTTP endpoint.

The part that is easy to get wrong is not the table, it is the transaction: the
stored response and the write that produced it commit **together**, in one
transaction opened by the middleware and joined by the repository. Two
transactions cannot do it — whichever commits second can fail, and then either a
key promises a response for a task that does not exist, or a task exists whose
key is stuck half-written and whose retry is refused until the TTL expires.

```bash
task demo:idempotency
```

## Rate limiting and circuit breaking

**A rate limiter protects you from your callers; a circuit breaker protects you
from your dependencies.** They point in opposite directions and are not
substitutes.

Inbound, two tiers:

- An **in-flight semaphore**, which is the one that actually keeps the process
  alive. A rate limiter bounds arrivals and says nothing about how many are
  still running, and 200 arrivals per second of ten-second requests is two
  thousand concurrent goroutines.
- A **token bucket per caller**, per tier, with TTL eviction — because a plain
  map keyed by caller address is an unbounded allocation driven by attacker
  input. That is the bug in most published implementations.

The caller's identity comes from a bearer token, and it is a *quota dimension*,
not an auth system: no scopes, no 403, and the default mode never returns 401,
because a reviewer whose first `curl` is refused concludes the exercise is
broken. Proxy hops are counted from the **right** of `X-Forwarded-For`; trusting
the leftmost entry is attacker-controlled and makes the limiter decorative.

Outbound, the chain is `retry(breaker(timeout(call)))` and the order changes the
semantics completely:

- **Timeout innermost** — a breaker with no per-attempt timeout never trips,
  because the calls do not fail, they hang.
- **Breaker inside the retry** — so every attempt is accounted. The other way
  round hides three failures behind one data point, and still compiles.

And the pairing that makes both worth having: **an open circuit becomes a
snooze**, which reschedules the job without consuming an attempt. The breaker
turns a slow failure into a fast one; on its own that is *worse* for the job,
which would burn its whole retry budget in milliseconds of instant refusals and
be discarded for an outage that had nothing to do with it.

```bash
task demo:ratelimit
task demo:breaker      # max attempt used: 1 of 5, through a 30-second outage
```

## Observability

Two listeners, and which port a thing answers on is the security boundary rather
than a convention. `:8080` is the API and nothing else. `:9090` carries
`/metrics`, `/healthz`, `/readyz`, `/debug/config`, `/debug/log-level` and pprof —
a heap profile is a memory dump, and an end-to-end test asserts each of those is
*absent* from the public port.

RED for the API, USE for the pools, and a vocabulary of its own for the queue,
where **the headline number is not throughput but the age of the oldest pending
job**. Depth alone cannot tell ten thousand jobs draining in twenty seconds from
five stuck for an hour, and reports the first as the emergency.

Cardinality is bounded by construction. Every label has a value set that can be
named and counted: the route is the templated path (`/tasks/{id}`), never the raw
URL; the status is the numeric code; the tier is one of three. No task ids, no
error strings, and no IP addresses anywhere — one scanner walking
`/tasks/<random-uuid>` would otherwise mint a time series per request.

Three dashboards and fourteen alerts are provisioned, so they exist the moment
`task up` finishes:

```bash
task up && task load
task dashboards:check     # runs all 49 panel queries against the live Prometheus
```


That last command exists because a dashboard whose panels return nothing is worse
than no dashboard — it reads as "the system is idle" rather than "this query is
wrong". It found ten broken panels the first time it ran.

The SLO alerts are multi-window multi-burn-rate (14.4×/6×/3×/1×) out of the SRE
workbook. A single-window alert either fires on a five-minute blip or arrives six
hours after the incident ended.

### The three signals join

A metric says the p99 moved and cannot say which request. A trace says where the
time went in one request and cannot say whether that request was typical. A log
says a thing happened. Separately they are three places to look; joined they are
one investigation.

- Every log record written inside a sampled span carries `trace_id` and
  `span_id`. A handler does it, not the call sites, because the attribute that
  has to be remembered is the one missing from the line that mattered.
- Every latency histogram carries **exemplars** — a pointer from a bucket to a
  span that landed in it. Grafana draws them as diamonds under the line, and the
  datasource maps `trace_id` into Tempo, so a spike is one click from its trace.
- Tempo links back the other way, from a span to the metrics around it.

`task up` starts Tempo alongside the rest. A request trace looks like this, and
the transactional outbox is visible in it — one transaction, two inserts:

```
POST /tasks                8.39 ms
├─ pool.acquire            0.02 ms
├─ BEGIN                   1.37 ms
├─ INSERT                  0.47 ms      the task
├─ INSERT                  0.72 ms      its event, same transaction
├─ COMMIT                  1.46 ms
└─ SELECT                  0.43 ms      pg_notify, after the commit
```

And the job, in the worker process, with the delivery nested inside it:

```
job task.event            11.54 ms   job.id=1 job.kind=task.event
└─ HTTP POST               7.36 ms   job.attempt=1 job.outcome=succeeded
```

Sampling is `ParentBased`, so a decision made upstream is honoured: a trace
sampled at the edge and dropped here is a trace with a hole in it.

## Operability

- **Configuration** merges four layers with provable precedence:
  `--flags` > `TASKAPI_*` > `config.yaml` > defaults. Every knob is reachable by
  all three spellings and they are the same knob: `--http.addr`,
  `TASKAPI_HTTP_ADDR` and `http: {addr: …}` all resolve to `http.addr`.
- **`--print-config`** dumps the effective merge with secrets masked, which
  answers "which configuration is it actually running" — a different question
  from "what is in the config file", and the one that is usually wrong.
- **SIGHUP** reloads the hot set, all-or-nothing, and logs every change by name
  with its old and new value. Load-time fields (a listen address, the storage
  backend) are refused loudly rather than silently ignored.
- **Three log tiers** — INFO, `-v` for DEBUG, `--trace` for wire-level. One wide
  canonical record per request rather than five scattered lines. Repeated
  warnings collapse to one per window with a `suppressed=n` count, because a
  flapping breaker should not bury the six lines that explain the incident.
- **Graceful shutdown** is a single ordered table in
  [`internal/app/app.go`](../internal/app/app.go). Read top to bottom it *is* the
  drain sequence: readiness flips first so a load balancer stops routing here
  before the listener stops accepting, and admin goes last so metrics answer for
  the whole drain instead of going dark at the start of it.

---

# What this does not do
