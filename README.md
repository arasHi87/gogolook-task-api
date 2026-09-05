# Task API

A task-list HTTP API in Go, and — beyond the exercise — the queue, the guards and
the observability that would let someone else operate it.

```bash
go run ./cmd/taskapi all      # no Docker, no database, no configuration
curl localhost:8080/tasks
```

---

## The exercise

| Required | Where it is | Where it is proved |
|---|---|---|
| `GET /tasks` — list | [`proto/task/v1/task.proto:134`](proto/task/v1/task.proto) | [`internal/runtime/contract_test.go`](internal/runtime/contract_test.go) |
| `POST /tasks` — create | same | same |
| `PUT /tasks/{id}` — update | same | same |
| `DELETE /tasks/{id}` — delete | same | same |
| `name` (string), `status` (int, `[0,1]`) | [`task.proto:84`](proto/task/v1/task.proto) | `status: 2` is rejected, in the same test |
| Go 1.18+ | `go.mod` says **1.26** — see [A note on the Go version](#a-note-on-the-go-version) | |
| Unit tests | 343 tests across 27 packages | `task test` |
| Dockerfile | [`Dockerfile`](Dockerfile) — distroless, non-root, 35 MB | `task docker:build` |
| In-memory storage | the default; Postgres is opt-in | `go run ./cmd/taskapi all` |

`internal/runtime/contract_test.go` is the file to open first. It runs every case
twice — once against `/api/v1/tasks` and once against the unversioned `/tasks`
the exercise specifies — and asserts the status, the JSON shape and the error
behaviour of all four endpoints. If that file passes, the requirement is met.

## Run it

Nothing but Go:

```bash
go run ./cmd/taskapi all
```

That is the whole quickstart, and the in-memory backend is the default for the
reason the exercise gives: storage may be in-memory, so a reviewer should not
need a database to see the endpoints work.

The four endpoints, against a running server:

```bash
# create
$ curl -sS -X POST localhost:8080/tasks \
    -H 'Content-Type: application/json' -d '{"name":"buy milk","status":0}'
{"id":"074eed42-7f66-4cfb-9252-9b3f715a84c3","name":"buy milk","status":0,
 "createdAt":"2026-09-05T16:23:47.852939Z","updatedAt":"2026-09-05T16:23:47.852939Z","version":"1"}

# list
$ curl -sS 'localhost:8080/tasks?page_size=2'
{"result":[ ... ],"nextPageToken":"MjAyNi0wOS0wNVQxNjoxNzo0OS4wNzIxMTVa..."}

# update
$ curl -sS -X PUT localhost:8080/tasks/074eed42-7f66-4cfb-9252-9b3f715a84c3 \
    -H 'Content-Type: application/json' -d '{"name":"buy milk","status":1}'

# delete
$ curl -sS -X DELETE localhost:8080/tasks/074eed42-7f66-4cfb-9252-9b3f715a84c3
```

`status` is validated, not merely documented:

```bash
$ curl -sS -X POST localhost:8080/tasks \
    -H 'Content-Type: application/json' -d '{"name":"nope","status":2}'
{"code":3,"message":"validation error: task.status: must be in list [0, 1]", ...}   # 400
```

Deleting an id that is already gone is a 404, not a silent success.

There is also `/docs` (Swagger UI) and `/openapi.yaml`, both generated from the
same proto the server is built from.

## In Docker

```bash
docker build -t taskapi .
docker run --rm -p 8080:8080 -p 9090:9090 taskapi
```

The image is `gcr.io/distroless/static-debian12:nonroot` — no shell, no package
manager, no libc, non-root by default. Its healthcheck is the binary probing
itself (`/taskapi healthcheck`), because there is no curl in there to call.

The full stack — Postgres, the API and the worker as separate processes, a
webhook receiver, Prometheus and Grafana — is one command:

```bash
task up      # then http://localhost:3000 for the dashboards
```

## Tests

```bash
task test              # unit only, no Docker, a few seconds
task test:integration  # adds a real Postgres via testcontainers
task e2e               # builds the binary and runs it as two processes
task verify            # all of the above, plus vet and lint
```

343 tests. **83.5%** statement coverage with the integration suite, 63% from unit
tests alone; the gap is the code that only runs against a real database, which is
the code most worth testing against one. Per-package: `apperr` 99%, `config` 94%,
`api` 93%, `pgrepo` 89%, `queue` 85%, `app` 61%, `cli` 55%.

The suites are three tiers, and each exists because the one below it cannot reach
the failure:

- **Unit** — no Docker. Config precedence, backoff bounds, error classification,
  the limiter's eviction, redaction.
- **Integration** — a real Postgres, because `SKIP LOCKED`, lease expiry, partial
  unique indexes and transaction visibility are properties of the database rather
  than of our code. A mock would assert that we called the methods we wrote.
- **[End-to-end](test/e2e/README.md)** — the shipped binary, run as two processes
  against a real database, driven over HTTP. It covers the seams the other two
  cannot: flag parsing, the config merge, signal handling, the drain, and a job
  crossing a process boundary. It is also the only place a lease can be tested
  honestly, because only a real process can be killed.

Nothing in the queue suite sleeps. Every wait is on a completion event, a log
record or a row, with a deadline that reports what it last saw — a queue suite
full of `time.Sleep` is the one everybody eventually skips.

---

# Beyond the requirements — and why

Everything above satisfies the exercise. Everything below is deliberate extra,
and each piece is here because of a specific failure it prevents. If you are
grading against the checklist, you can stop reading here.

## The contract comes from one source

`proto/task/v1/task.proto` declares the messages, the validation rules and the
HTTP mapping. From it, [buf](buf.gen.yaml) generates the Go types, the Connect
handler and `docs/openapi.yaml`; at runtime, [vanguard](internal/runtime/handler.go)
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

It is written rather than imported, and [`internal/queue/README.md`](internal/queue/README.md)
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
which is the correct answer rather than a tolerated flaw; see
[at-least-once](#at-least-once-is-the-guarantee-not-a-shortcut) below.

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
  [`internal/app/app.go`](internal/app/app.go). Read top to bottom it *is* the
  drain sequence: readiness flips first so a load balancer stops routing here
  before the listener stops accepting, and admin goes last so metrics answer for
  the whole drain instead of going dark at the start of it.

---

# What this does not do

## At-least-once is the guarantee, not a shortcut

Exactly-once *effects* are achievable; exactly-once *delivery* is not. The
webhook receiver is an external system we cannot open a transaction with, so a
worker that crashes between "the HTTP call succeeded" and "the row says
succeeded" will deliver again. `task demo:crash` shows exactly that, on purpose,
rather than picking a demo that hides it. The honest answer is to send
`X-Event-Id`, document that the receiver must deduplicate on it, and say so out
loud.

## The rate limiter is per replica

Three replicas behind a load balancer enforce three times the nominal limit. Real
deployments enforce this at the edge — Envoy, nginx, Cloudflare — with a shared
counter. What is here is the backstop for when the edge is misconfigured, not the
primary control.

## An idempotency key has a crash window of its own

The key, the task and the stored response commit in one transaction, so there is
no half-written state. But a client that retries *after* the 24-hour TTL has
expired will execute again. That is what a TTL means, and it is why the draft has
one.

## There is no tracing collector

`trace_id` plumbing and OTLP configuration exist; nothing is exported, because
the collector is a stretch goal that was not reached. Exemplars linking a latency
spike to the trace that caused it are the demo that would land, and they are not
here.

## Other things worth naming

- **No auth worth the name.** A static bearer token resolving to a client id and
  a tier. It exists to give the limiter and the dashboards a real tenant
  dimension. There are no scopes and no authorisation.
- **No per-query database metrics.** The pool's saturation is reported; a
  latency breakdown by operation would need a pgx tracer on every statement. A
  metric defined and never written draws an empty panel that reads as "the
  system is idle", so it is absent rather than hollow.
- **`jobs` is not partitioned.** At the volume where the claim index stops fitting
  in memory, the move is monthly partitions on `created_at`, or archiving
  finalized rows to a history table. The purger keeps it honest until then.
- **Load testing is not included.** The suites prove behaviour, not throughput.

## A note on the Go version

The exercise says Go 1.18+; `go.mod` requires **1.26**, for range-over-int and
the `slog`/`testing` APIs used throughout. Any Go newer than 1.26 builds it, and
the Docker path needs no local Go at all:

```bash
docker build -t taskapi . && docker run --rm -p 8080:8080 taskapi
```

If a specific older toolchain is a hard requirement, say so — the changes are
mechanical.

---

# Layout

```
cmd/taskapi/          the binary
proto/                the contract: messages, validation, HTTP mapping
gen/                  generated from proto — never edited by hand
docs/                 the generated OpenAPI document, and Swagger UI
migrations/           versioned SQL, embedded in the binary
internal/
  app/                the composition root, and the drain-order table
  api/                the four RPC handlers, and nothing else
  runtime/            transport: the mux, transcoding, the middleware chain
  task/               the domain, its repository port, and two adapters
  queue/              claim, lease, heartbeat, reaper, scheduler, purge
  outbox/             the enqueue that runs inside a caller's transaction
  postgres/           pool, migrations, advisory locks, ambient transactions
  idempotency/        the Idempotency-Key layer and its two stores
  auth/ ratelimit/    inbound guards: who you are, and how much you may have
  resilience/         outbound guard: timeout, retry, circuit breaker
  metrics/            the Prometheus surface, and the scrape-time collectors
  admin/              health, readiness, and the debug endpoints
  config/  logging/   one file per config section; three-tier structured logs
  httpx/  apperr/     middleware primitives; the shared error vocabulary
deploy/               compose, Prometheus rules, provisioned Grafana
test/e2e/             the live suite
test/demo/            crash, idempotency, rate limit, breaker
```

# Every task

```bash
task                    # list them
task run                # the API on the in-memory store
task ci                 # tidy, vet, lint, unit tests
task verify             # ci, then the suites that need Docker
task up / down          # the full stack
task load               # traffic, so the dashboards have something on them
task demo:crash         # SIGKILL a worker mid-job; watch the reaper
task demo:idempotency   # send the same write twice; watch it happen once
task demo:ratelimit     # two tiers, and what a refused caller is told
task demo:breaker       # break the dependency; watch the circuit open
task generate           # regenerate from proto
task dashboards:check   # every panel query against the live Prometheus
```
