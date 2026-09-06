# Gogolook BE Coding Exercise — Architecture Plan

> **Status: DRAFT FOR DISCUSSION.** Nothing here is implemented. Once we agree section by
> section, this file becomes the implementation brief for a fresh session.

---

## §0 — Scope, and an honest framing note

### What the exercise actually asks for

From `BE_Coding Exercise_2B.pdf` (Assignment – Senior Backend Engineer):

| Endpoint | Meaning |
|---|---|
| `GET /tasks` | list tasks |
| `POST /tasks` | create a task |
| `PUT /tasks/{id}` | update a task |
| `DELETE /tasks/{id}` | delete a task |

Task fields (**at least**): `name` (string, task name), `status` (integer, enum `[0,1]`,
0 = incomplete, 1 = completed).

Requirements: endpoints work as expected · Go 1.18+ · unit tests · Dockerfile to run the API in
Docker · GitHub repo link · **"for data storage, you can use any in-memory mechanism"**.

### The framing risk, stated once

What we are planning is roughly 20× the literal ask. That is a deliberate choice — the grader for a
*senior* slot is reading for judgment and production instincts, not for whether you can write a map
with a mutex. But over-engineering is a real failure mode in these exercises, and the way it goes
wrong is: the reviewer clones the repo, `docker compose up` fails on their machine, and they never
see any of it.

Three mitigations, baked into the plan and non-negotiable:

1. **The literal requirement stays satisfiable with zero dependencies.** The repository layer is an
   interface with two implementations. `--storage=memory` (the *default*) runs the whole API with no
   Postgres, no Docker, no compose — `go run ./cmd/taskapi serve` works on a plane. That is literally
   what the PDF asked for. `--storage=postgres` unlocks everything else.
2. **`README.md` opens with a 3-line quickstart, then a "Beyond the requirements — and why" section**
   that indexes the extras. The reviewer is told what to look at and can stop whenever they like.
3. **Every extra is load-bearing for a story**, not a checklist item. See §0.2.

### §0.1 — The domain, extended just enough

`Task` stays exactly as specified (`id`, `name`, `status`) plus the housekeeping every real table
has (`created_at`, `updated_at`, and `version` for optimistic concurrency). We do **not** invent
business fields.

What we *do* add is one behaviour: **task state changes emit events**, and those events are
delivered asynchronously to a configurable webhook. That single addition is the spine that every
other section hangs off.

### §0.2 — Why that one addition justifies everything else

```
                 ┌──────────── inbound: needs a RATE LIMITER (§5) ───────────┐
  client ──HTTP──►  API (§1,§7)  ──tx──►  Postgres: tasks + outbox (§4)
                 └──────────────────────────────────────────────────────────┘
                                              │  LISTEN/NOTIFY + poll
                                              ▼
                                    worker pool (§4) ──HTTP──► webhook sink
                                              │                    ▲
                                              │                    │
                              lease/heartbeat/reaper      outbound: needs a
                              retry/backoff/DLQ           CIRCUIT BREAKER (§5)
```

- The **outbox** is why we need a transaction, and therefore why in-memory storage is not enough.
- The **queue** is why we need leases, heartbeats, idempotency, and a reaper.
- The **webhook** is a flaky remote dependency — the only honest place a circuit breaker belongs.
- The **inbound API** is the only honest place a rate limiter belongs.
- All of it produces the numbers that make §6's dashboards non-fictional.

Without the webhook there is nothing to break a circuit on, and the resilience section becomes
theatre. **§D confirms this shape — D0 is decided: yes.**

### §0.3 — Two commands, one binary

`cmd/taskapi` is a cobra root (POSIX flags, matching lachesis' `scenariotest`):

| Subcommand | Role |
|---|---|
| `serve` | HTTP API only (no queue consumption) |
| `worker` | queue consumer only (no public listener; admin listener still up) |
| `all` | both in one process — the default for `go run` and single-container demos |
| `migrate up/down/status` | schema migration (advisory-lock guarded) |
| `healthcheck` | probes `/readyz` and exits 0/1 — distroless has no shell for `curl` (§8.1) |
| `version` | build info |

Splitting `serve` from `worker` in compose is what demonstrates that the queue is genuinely
decoupled — and it makes "kill the worker mid-job, watch the lease reaper" a live demo (§4.4, §8.3).

### §0.4 — Repository

```
git@github.com:arasHi87/gogolook-task-api.git
module github.com/arasHi87/gogolook-task-api
```

Go module paths are **case-sensitive** and must match the GitHub owner exactly — `arasHi87`, capital
H. Getting this wrong surfaces as a `parsing go.mod: module declares its path as …` error the first
time anyone runs `go get`, so it is worth typing carefully once. (The module proxy escapes the
uppercase letter as `!h` in cache paths; that is normal and needs no action.)

Branch protection is not needed for a solo exercise, but **CI must be green on `main`** — the badge
is the first thing in the README and a red one costs more than the workflow saves.

---

## §D — Decisions — **all closed, 2026-09-05**

Every fork below is settled. Nothing in this plan is waiting on an answer; a fresh session can
implement straight from §11's build order.

| # | Decision | My recommendation |
|---|---|---|
| ~~D0~~ | ~~outbox → webhook spine~~ | ✅ **DECIDED: yes.** Tasks emit events in-tx; worker delivers to webhook (§0.2) |
| ~~D1~~ | ~~go-micro vs connect-go~~ | ✅ **DECIDED: drop go-micro.** `net/http` + connect-go + vanguard (§1.4) |
| ~~D1b~~ | ~~URL versioning~~ | ✅ **DECIDED: `/api/v1/*` canonical, bare `/tasks` dual-bound as deprecated alias** (§7.4) |
| ~~D2~~ | ~~vanguard alpha risk~~ | ✅ **DECIDED: accept vanguard**, chi adapter kept as the escape hatch (§1.5) |
| ~~D3~~ | ~~config lib~~ | ✅ **DECIDED: `knadh/koanf`** + pflag + cobra (§3.1) |
| ~~D4~~ | ~~roll our own queue vs river~~ | ✅ **DECIDED: build it ourselves**, mining river for 12 specific mechanisms. Census in **§4.8**; README framing in §4.9 |
| ~~D5~~ | ~~breaker lib~~ | ✅ **DECIDED: `failsafe-go`** — composes retry+timeout+breaker+bulkhead as one ordered chain (§5.5) |
| ~~D6~~ | ~~tracing scope~~ | ✅ **DECIDED: stretch goal.** `trace_id` plumbing lands at Milestone 2 regardless; collector + exemplars only if Milestone 10 finishes early (§6.7, M12) |
| ~~D7~~ | ~~module path~~ | ✅ **DECIDED: `github.com/arasHi87/gogolook-task-api`** — repo exists (§0.4) |
| ~~D8~~ | ~~auth~~ | ✅ **DECIDED: static bearer token, `mode: optional` by default** — tiered limiter, never 401s a reviewer's first curl (§5.2a) |

---

## §1 — Framework and the HTTP surface

*(This section and §7 are one decision, not two. Read them together.)*

### §1.1 — The tension

You asked for go-micro (§1) and for protobuf-generated OpenAPI (§7), and then asked whether the
required paths survive. They are in direct conflict, and the conflict is worth naming precisely.

**go-micro is an RPC mesh framework, not an HTTP framework.** Its core model is
`func (t *Tasks) List(ctx, *Req, *Rsp) error`, served over its own transport, discovered through a
registry (mDNS by default), addressed as `Tasks.List`. Its REST story is the separate `micro api`
gateway, which maps `http://host:8080/api/{service}/{method}` — i.e. `/api/tasks/list`, **not**
`GET /tasks`. Getting `PUT /tasks/{id}` out of it means either the gateway's path-rewrite rules or
hand-written routing.

There is a second, quieter problem. If you *do* mount a plain `net/http` mux inside go-micro (which
is supported — `go-micro.dev/v5/server/handler/transport/http.Handle(pattern, http.Handler)`), then
go-micro's `wrapper/breaker` and `wrapper/ratelimiter` plugins **do not apply**, because they wrap
the RPC handler chain, not the HTTP one. So the single strongest reason to bring go-micro in (§5:
"go-micro provides limiter and breaker") evaporates exactly when you configure it to serve the
required paths.

Net: on this exercise, go-micro would contribute service lifecycle, config, and a registry we have
no second service to discover — while we work around it for routing, middleware, limiting, breaking,
and OpenAPI.

### §1.2 — What connect-go + vanguard gives instead

```
proto/task/v1/task.proto          ← the ONE source of truth
  ├─ protoc-gen-go              → Go structs
  ├─ protoc-gen-connect-go      → Connect/gRPC/gRPC-Web handler + client
  ├─ protoc-gen-connect-openapi → docs/openapi.yaml  (OpenAPI 3.1, honours google.api.http)
  └─ protovalidate (buf.validate) → runtime validation AND documented constraints
                                    (status enum [0,1] declared once, enforced everywhere)

runtime: vanguard.NewTranscoder(...) reads the same google.api.http annotations
         → serves GET /tasks, POST /tasks, PUT /tasks/{id}, DELETE /tasks/{id}
           on one mux, one port, plus gRPC and Connect clients for free
```

The proto declares the REST mapping inline. Canonical paths are **`/api/v1/*`**; the bare paths from
the PDF are dual-bound as deprecated aliases via `additional_bindings` (rationale in §7.4):

```proto
package task.v1;                     // the version lives in the IDL too

service TaskService {
  rpc ListTasks(ListTasksRequest) returns (ListTasksResponse) {
    option (google.api.http) = {
      get: "/api/v1/tasks"
      additional_bindings { get: "/tasks" }                          // deprecated alias
    };
  }
  rpc CreateTask(CreateTaskRequest) returns (CreateTaskResponse) {
    option (google.api.http) = {
      post: "/api/v1/tasks"  body: "task"
      additional_bindings { post: "/tasks"  body: "task" }
    };
  }
  rpc UpdateTask(UpdateTaskRequest) returns (UpdateTaskResponse) {
    option (google.api.http) = {
      put: "/api/v1/tasks/{id}"  body: "task"
      additional_bindings { put: "/tasks/{id}"  body: "task" }
    };
  }
  rpc DeleteTask(DeleteTaskRequest) returns (DeleteTaskResponse) {
    option (google.api.http) = {
      delete: "/api/v1/tasks/{id}"
      additional_bindings { delete: "/tasks/{id}" }
    };
  }
}
```

So the answer to your §7 question — *"can the path still follow the convention in the requirement?"*
— is **yes, exactly**, and the OpenAPI document is generated from the same annotations, so the doc
and the router are structurally incapable of drifting. That is the property you actually wanted.

### §1.3 — The alternatives considered

| Approach | Required paths | OpenAPI | Cost |
|---|---|---|---|
| go-micro RPC + `micro api` gateway | ✗ `/api/tasks/list` shape | ✗ hand-written | extra process, wrong URLs |
| go-micro + mounted `net/http` mux | ✓ (you route it) | ✗ hand-written or separate | go-micro reduced to a DI container; its wrappers unused |
| **connect-go + vanguard-go** | ✓ from annotations | ✓ OpenAPI **3.1** generated | vanguard is alpha (§1.5) |
| gRPC + grpc-gateway v2 | ✓ from annotations | ✓ but OpenAPI **2.0** (swagger) | two servers or in-process handler, two codegen paths, older spec |
| chi + hand-written handlers + hand-written OpenAPI | ✓ | ✓ but **drifts** | exactly the problem you asked to avoid |

grpc-gateway is the conservative pick and is genuinely battle-tested; the reasons I still prefer
vanguard are (a) OpenAPI 3.1 vs 2.0, (b) one mux / one port / no gRPC server needed, (c) no
generated gateway code to check in and keep in sync.

### §1.4 — DECIDED (D1)

**Drop go-micro. Use `net/http` + connect-go + vanguard-go.**

The argument to make in the README — and in the interview — is *"go-micro solves service discovery,
brokered pub/sub, and RPC-mesh concerns for a fleet of services. This is one service with an
externally-specified REST contract. Adding it here would mean fighting the framework for the URLs
in the spec, and its resilience wrappers would not even be on the path that serves them. Choosing
not to use a framework, and saying why, is the senior answer."*

**If go-micro is a hard requirement** (it's in the JD, Gogolook uses it internally, you want to show
familiarity), the fallback is coherent and I'll plan it instead: go-micro as the **composition root
only** — `micro.NewService()` for lifecycle/graceful-shutdown/config/registry, with the
vanguard+connect mux mounted through `server/handler/transport/http.Handle("/", mux)`. Everything in
§2–§8 is unchanged; only the wiring in `cmd/` and one section of the README differ. Say the word.

> One note if we do go go-micro: `go-micro.dev/v5` is at v5.30.0 (Jun 2026) but the highest tagged
> major is now **v6** (v6.13.0, Aug 2026), which has pivoted hard toward "agent harness" / LLM
> tooling. We would pin v5 and say why.

### §1.5 — The vanguard alpha risk, and the escape hatch (D2)

`connectrpc/vanguard-go` says plainly: *"undergoing initial development and is not yet stable."* It
is a Buf/ConnectRPC project and it works, but the API can move.

Escape hatch, decided up front so it is never a scramble: **the domain service
(`internal/task.Service`) is protocol-agnostic** — plain Go types, plain Go errors, no proto in its
signatures. `internal/api` is a thin adapter (proto ⇄ domain). If vanguard ever gets in the way, a
~100-line `chi` router calling the same `task.Service` replaces it, and the only thing lost is the
generated-vs-served guarantee (which we'd then backstop with the contract test in §10.4). The blast
radius is one package.

### §1.6 — Error mapping

Connect errors carry a code; vanguard maps them to HTTP status. We map domain → connect explicitly
in one place (`internal/api/errors.go`), never by leaking `sql.ErrNoRows`:

| Domain error | Connect code | HTTP |
|---|---|---|
| `ErrNotFound` | `CodeNotFound` | 404 |
| `ErrInvalidArgument` / protovalidate failure | `CodeInvalidArgument` | 400 |
| `ErrConflict` (optimistic-lock version mismatch) | `CodeAborted` | 409 |
| `ErrIdempotencyKeyReuse` (same key, different body) | `CodeInvalidArgument` | 422 |
| `ErrUnauthenticated` (`auth.mode: required`, bad/absent token) | `CodeUnauthenticated` | 401 + `WWW-Authenticate` |
| rate limited | `CodeResourceExhausted` | 429 |
| breaker open / dependency down | `CodeUnavailable` | 503 |
| anything unhandled | `CodeInternal` | 500 |

Body shape: RFC 9457 `application/problem+json` for the REST surface if vanguard lets us shape it;
otherwise Connect's native error envelope, documented in the OpenAPI. **Never** echo an internal
error string to the client — log it with the `request_id`, return the id.

---

## §2 — Logging (slog, three tiers)

Mirrors the lachesis convention directly, so the levels mean the same thing they mean in your other
codebase.

### §2.1 — The tiers

`TRACE` is `slog.Level(-8)` — the conventional "Debug − 4" slot on slog's numeric scale, same as
`scenariotest.LevelTrace`.

| Flag | Tier | What you see |
|---|---|---|
| *(none)* | `INFO`+ | the **lifecycle narrative** only |
| `-v` / `--debug` | `DEBUG`+ | + per-request and per-job lifecycle |
| `--trace` | `TRACE`+ | + the wire (SQL, outbound HTTP, pool, heartbeats) |

### §2.2 — What belongs at each level (the actual contract)

**`ERROR` — a human may need to act.**
Unhandled panic recovered · job `discarded` to DLQ after max attempts · DB unreachable after retry
budget · config reload rejected · listener failed to bind · migration failed.

**`WARN` — degraded, but self-healing.**
Circuit breaker → `open` · job retry scheduled (with `next_at`) · lease expired and job reclaimed by
the reaper · sustained rate-limit shedding (rate-limited log line itself, once per 10 s) · slow
query over threshold · restart-only config field changed on SIGHUP (`load-time field change ignored`,
verbatim from lachesis) · readiness flipped to not-ready.

**`INFO` — the default. One line per *state change*, never per event.**
Boot banner (version, commit, go version, effective-config digest) · migrations applied (n) ·
listeners up (`api=:8080 admin=:9090`) · storage backend selected · worker pool started (n workers) ·
`ready` · `SIGHUP reload applied` + one `tunable changed field=… from=… to=…` per field · drain
started / drain complete (in-flight n) / shutdown.

> A request log line at INFO is the single most common mistake here. At 1000 rps that is 1000
> lines/s of noise that hides the six lines that matter. Requests are a **metric**, not a log.

**`DEBUG` (`-v`) — per-request, per-job.**
One **canonical log line** per request (Stripe's pattern — one wide structured record, not five
scattered ones): `method`, `route` (templated), `status`, `dur_ms`, `bytes_in/out`, `client_id`,
`request_id`, `trace_id`, and any `error`. Per-job: `claimed` (id, kind, attempt) → `succeeded`
(dur) | `retry scheduled` (next_at, err) | `discarded`. Breaker state transitions with counts.
Limiter decisions (sampled). Config values applied at boot.

**`TRACE` (`--trace`) — the wire.**
Every SQL statement (**text + duration + rows; args redacted**), every outbound HTTP call
(`method`/`url`/`status`/`dur` — **never bodies**), claim-query batch sizes, heartbeat ticks, pgx
pool acquire/release, NOTIFY receipts, reaper sweep results.

### §2.3 — Rules

- **Bodies are never logged at any level.** Same rule as lachesis: request/response bodies, DSNs
  with passwords, bearer tokens, `Idempotency-Key` values (log a truncated SHA-256 instead).
  Redaction is enforced by a `slog.HandlerOptions.ReplaceAttr` denylist, not by discipline.
- **stderr, always.** Keeps the lachesis convention and leaves stdout free for machine data
  (`--print-config`, `migrate status --json`). Docker captures both.
- **Handler by TTY:** `charmbracelet/log` when stderr is a terminal (colour, `NO_COLOR` honoured);
  `slog.JSONHandler` when piped or when `--log-format=json`.
- **Every record carries** `service`, `version`, and — when in a request/job scope — `request_id`,
  `trace_id`, `span_id`, `job_id`. Delivered via a context-scoped logger, not globals.
- **Runtime level switching** through a single `slog.LevelVar`: settable by flag, by config, by
  SIGHUP, and by `PUT /debug/log-level` on the admin listener (lachesis parity).
- **Hot paths use `logger.LogAttrs(ctx, lvl, msg, attrs...)`** to avoid `any` boxing, and guard
  expensive field construction with `logger.Enabled(ctx, LevelTrace)`.
- **Log flooding is bounded:** repeated identical WARN/ERROR (breaker flapping, DB down) go through
  a small per-key rate limiter — first occurrence, then once per 10 s with a `suppressed=n` count.
- Package: `internal/logging` (Library archetype, per §9).

---

## §3 — Configuration

### §3.1 — Precedence

`CLI flags` > `environment` > `config.yaml` > `built-in defaults` — as you specified, and matching
lachesis.

Implementation (**D3**): **`knadh/koanf`** + `spf13/pflag` + `spf13/cobra`. Load order is literally
the precedence, last-wins:

```go
k.Load(structs.Provider(Defaults(), "koanf"), nil)                     // 1. defaults
k.Load(file.Provider(path), yaml.Parser())                             // 2. config.yaml   (if present)
k.Load(env.Provider("TASKAPI_", ".", envToPath), nil)                  // 3. environment
k.Load(posflag.Provider(flags, ".", k), nil)                           // 4. --flags       (changed-only)
k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{Tag: "koanf"})
```

Two details that make this actually correct rather than nearly correct:

- Step 4 must only apply flags the user **actually set** (`flags.Changed`). Otherwise pflag defaults
  overwrite YAML and env, and the precedence silently inverts. This is the classic viper bug.
- Step 3's `envToPath` maps `TASKAPI_HTTP_ADDR` → `http.addr`, i.e. `_` → `.` under the prefix. One
  function, unit-tested, so `--http.addr`, `TASKAPI_HTTP_ADDR`, and `http: {addr: …}` are provably
  the same knob.

*Why not viper:* globals, magic, and a long history of precedence surprises. *Why not go-micro
config:* it merges last-wins too and would work fine — but it only makes sense if D1 keeps go-micro.

### §3.2 — Shape (one struct, three spellings, no drift)

```yaml
service:
  name: taskapi
logging:
  level: info          # error|warn|info|debug|trace
  format: auto         # auto|text|json
http:
  addr: ":8080"
  read_header_timeout: 5s
  read_timeout: 15s
  write_timeout: 30s
  idle_timeout: 60s
  shutdown_grace: 20s
  max_body_bytes: 1048576
  trusted_proxy_hops: 0     # how many X-Forwarded-For entries from the RIGHT to trust (§5.2a)
admin:
  addr: ":9090"        # /metrics /healthz /readyz /debug/* /debug/pprof
  pprof: true
storage:
  backend: memory      # memory|postgres
  postgres:
    dsn: ""            # env/secret only — never in the yaml, never logged
    max_conns: 20
    min_conns: 2
    max_conn_lifetime: 30m
    max_conn_idle_time: 5m
    health_check_period: 30s
    statement_timeout: 5s
    idle_in_transaction_session_timeout: 10s
queue:
  workers: 8
  claim_batch: 10
  lease: 30s            # visibility timeout
  heartbeat_interval: 10s   # == lease/3
  poll_interval: 2s     # LISTEN/NOTIFY fallback
  reaper_interval: 15s
  max_attempts: 5
  backoff: {base: 1s, max: 5m, jitter: full}
  retention: {succeeded: 24h, discarded: 168h, purge_interval: 1h}
auth:
  mode: optional            # optional|required|off  — see §5.2a
  clients:                  # token plaintext NEVER here; only its sha256
    - {id: demo,   tier: standard, token_sha256: "9f86d081…"}
    - {id: worker, tier: internal, token_sha256: "60303ae2…"}
ratelimit:
  enabled: true
  global_inflight: 200
  key_ttl: 10m
  tiers:
    anonymous: {rate: 10,   burst: 20}
    standard:  {rate: 100,  burst: 200}
    internal:  {rate: 1000, burst: 2000}
breaker:
  webhook:
    failure_rate_threshold: 0.5
    min_throughput: 20
    window: 10s
    open_duration: 30s
    half_open_max_calls: 5
    half_open_success_threshold: 3
webhook:
  url: ""
  timeout: 3s
  retry: {max_attempts: 3, base: 200ms, max: 5s}
observability:
  metrics: {enabled: true, buckets: default}
  tracing: {enabled: false, otlp_endpoint: ""}
```

### §3.3 — Operational surface (lachesis parity)

- **`--config <path>`**, POSIX two-dash only (pflag). Single-dash long flags do not parse — that's
  the deliberate cobra/pflag behaviour, called out in the README.
- **`--print-config`** dumps the *effective* merged config to stdout as YAML and exits 0. Secrets
  rendered as `«redacted»`.
- **`GET /debug/config`** on the admin listener renders the same live effective values.
- **SIGHUP hot-reload.** The **hot set**: `logging.level`, `queue.workers` (pool resize),
  `queue.poll_interval`, `queue.reaper_interval`, `queue.max_attempts`, `queue.backoff.*`,
  `ratelimit.*`, `breaker.*`, `webhook.timeout`/`retry.*`. **Restart-only**: `http.addr`,
  `admin.addr`, `storage.*`, `logging.format`, `observability.*` — a SIGHUP touching these logs a
  loud `load-time field change ignored` WARN and skips them.
- **Reload is all-or-nothing.** Parse + validate the whole new config first; on any error the running
  config is untouched and we emit `config reload rejected`. One `tunable changed field=X from=A to=B`
  INFO line per changed field, then `SIGHUP reload applied`.
- **`config_reloads_total{result="applied|invalid|read_error"}`** counter (§6).
- **Validation** is a method on the struct (`func (c *Config) Validate() error`) returning *all*
  problems joined, not the first — so a bad config file is fixed in one pass, not five.
- Package: `internal/config` (Library archetype).

---

## §4 — Postgres as database *and* job queue

**DECIDED (D4): build the queue ourselves (~400 lines), and mine `riverqueue/river` for the
mechanisms worth stealing.** The queue is the strongest thing in the submission to be able to
defend line by line; a `river.Config{}` literal is not. But river encodes six or seven hard-won
lessons that a from-scratch queue written in an afternoon reliably misses, so we take those
deliberately rather than rediscovering them in the interview. **§4.8 is that census** — read it
before writing any queue code.

This is the section with the most real content, and the one you flagged the hard parts of correctly:
heartbeat, keepalive, idempotency.

### §4.1 — Schema

```sql
-- the domain
CREATE TABLE tasks (
  id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),  -- pgcrypto; or UUIDv7
  name        text        NOT NULL CHECK (length(name) BETWEEN 1 AND 255),
  status      smallint    NOT NULL DEFAULT 0 CHECK (status IN (0,1)),
  version     bigint      NOT NULL DEFAULT 1,   -- optimistic concurrency
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX tasks_created_at_id_idx ON tasks (created_at DESC, id DESC);  -- keyset pagination

-- the queue (also the outbox — see §4.2)
-- State machine borrowed from river (§4.8 #1): `retryable` MUST be distinct from `available`,
-- or a failing job is instantly re-claimable and hot-loops the pool.
--
--            ┌──────────────── scheduler makes due jobs available ───────────────┐
--            ▼                                                                    │
--   available ──claim──► running ──ok──► succeeded                                │
--        ▲                   │                                                    │
--        │                   ├──retryable error──► retryable ──(scheduled_at)─────┘
--        │                   ├──terminal error───► cancelled          (no retry)
--        │                   ├──attempts spent───► discarded          (DLQ)
--        └──snooze (no attempt consumed)──┘
--
CREATE TYPE job_state AS ENUM
  ('available','scheduled','running','retryable','succeeded','cancelled','discarded');

CREATE TABLE jobs (
  id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  kind          text        NOT NULL,
  payload       jsonb       NOT NULL,
  state         job_state   NOT NULL DEFAULT 'available',
  priority      smallint    NOT NULL DEFAULT 0,
  attempt       int         NOT NULL DEFAULT 0,
  max_attempts  int         NOT NULL DEFAULT 5,
  scheduled_at  timestamptz NOT NULL DEFAULT now(),
  locked_by     text,                    -- worker id holding the lease
  locked_at     timestamptz,
  lease_expires_at timestamptz,
  heartbeat_at  timestamptz,
  attempted_by  text[]      NOT NULL DEFAULT '{}',   -- every worker that tried (river §4.8 #7)
  errors        jsonb       NOT NULL DEFAULT '[]'::jsonb,  -- full history, not just the last
  unique_key    text,                    -- dedup / idempotency
  created_at    timestamptz NOT NULL DEFAULT now(),
  finalized_at  timestamptz
);

-- the only index the claim query may use
CREATE INDEX jobs_claim_idx ON jobs (priority, scheduled_at, id)
  WHERE state = 'available';
-- the scheduler's index: retryable/scheduled rows that have come due
CREATE INDEX jobs_schedule_idx ON jobs (scheduled_at)
  WHERE state IN ('retryable','scheduled');
-- the reaper's own small index (running rows aren't in the partial indexes above)
CREATE INDEX jobs_lease_idx ON jobs (lease_expires_at)
  WHERE state = 'running';
-- enqueue-time dedup: one live job per logical key
CREATE UNIQUE INDEX jobs_unique_key_idx ON jobs (unique_key)
  WHERE unique_key IS NOT NULL
    AND state IN ('available','scheduled','running','retryable');
-- purge
CREATE INDEX jobs_finalized_idx ON jobs (finalized_at)
  WHERE state IN ('succeeded','cancelled','discarded');

-- high-churn table: tune autovacuum per-table, not globally
ALTER TABLE jobs SET (autovacuum_vacuum_scale_factor = 0.0,
                      autovacuum_vacuum_threshold  = 1000,
                      autovacuum_analyze_scale_factor = 0.0,
                      autovacuum_analyze_threshold = 1000,
                      fillfactor = 80);   -- room for HOT updates

-- API-level idempotency (§4.5)
CREATE TABLE idempotency_keys (
  key           text        PRIMARY KEY,
  fingerprint   bytea       NOT NULL,     -- sha256(method|path|body)
  state         text        NOT NULL,     -- 'in_progress' | 'completed'
  status_code   int,
  response_body jsonb,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL DEFAULT now() + interval '24 hours'
);
CREATE INDEX idempotency_keys_expiry_idx ON idempotency_keys (expires_at);
```

### §4.2 — Outbox and queue are the same table

The classic outbox pattern uses a separate `outbox` table plus a relay that republishes to a broker.
We have no broker — Postgres *is* the transport — so the outbox row and the job row are one row:

```go
func (r *Repo) CreateTask(ctx context.Context, t *task.Task) (*task.Task, error) {
    tx, err := r.pool.Begin(ctx)
    if err != nil { return nil, err }
    defer tx.Rollback(ctx)              //nolint:errcheck — no-op after Commit

    created, err := insertTask(ctx, tx, t)                    // 1. the domain write
    if err != nil { return nil, err }
    if err := r.q.Enqueue(ctx, tx, queue.Job{                 // 2. the event, SAME tx
        Kind:      "task.event",
        Payload:   taskEvent{ID: created.ID, Event: "task.created", Version: created.Version},
        UniqueKey: "task.created:" + created.ID.String(),
    }); err != nil { return nil, err }

    return created, tx.Commit(ctx)                            // 3. both, or neither
}
```

Either the task and its event both exist, or neither does. There is no window in which a task exists
with no event queued, and none in which an event fires for a task that rolled back. That is the
dual-write problem eliminated, not mitigated — and it is why the queue lives in Postgres rather than
Redis.

`Enqueue` takes a `pgx.Tx`, never a pool. That signature is the enforcement mechanism: it is not
*possible* to enqueue outside a caller's transaction.

Worth naming in the README: "outbox as a ledger, not a queue" is a live debate, and knowing which one
you built is the point. This is the queue variant, deliberately.

### §4.3 — The claim query

```sql
WITH claimed AS (
  SELECT id FROM jobs
   WHERE state = 'available' AND scheduled_at <= now()
   ORDER BY priority, scheduled_at, id
   FOR UPDATE SKIP LOCKED
   LIMIT $2
)
UPDATE jobs j
   SET state            = 'running',
       attempt          = j.attempt + 1,
       locked_by        = $1,
       locked_at        = now(),
       lease_expires_at = now() + $3::interval,
       heartbeat_at     = now(),
       attempted_by     = array_append(j.attempted_by, $1)
  FROM claimed c
 WHERE j.id = c.id
RETURNING j.id, j.kind, j.payload, j.attempt, j.max_attempts;
```

`FOR UPDATE SKIP LOCKED` (PG 9.5+) is what makes N workers safe: a row already locked by another
worker is skipped, not waited on. `ORDER BY … LIMIT` inside the CTE plus the partial index keeps it
an index scan.

Note `state = 'available'`, not "pending or retryable". Retryable jobs are made available by the
**scheduler** (§4.8 #2), not picked up directly — that separation is what stops a hot-failing job
from monopolising the claim query.

### §4.4 — Lease, heartbeat, reaper — the crash story

**This is the one place we deliberately do more than river**, so it is the strongest thing in §4 to
be asked about. River rescues stuck jobs on a static `RescueStuckJobsAfter` (default **1 hour**),
which cannot distinguish *slow* from *dead*. A heartbeat can: it is a positive liveness proof, so we
can reclaim after 30 s while still permitting a 10-minute job.

**Lease (visibility timeout).** A claim stamps `lease_expires_at = now() + 30s`. If the worker dies,
the row is stuck in `running` — but only until the lease lapses.

**Heartbeat.** Each worker runs one goroutine that, every `lease/3` (10 s), issues **one batched**
update for all of its in-flight ids:

```sql
UPDATE jobs SET heartbeat_at = now(), lease_expires_at = now() + $2::interval
 WHERE id = ANY($1) AND state = 'running' AND locked_by = $3;
```

`lease/3` is the standard choice: two consecutive missed heartbeats are tolerated before the reaper
acts (same reasoning as a Raft/etcd lease TTL vs keepalive interval). If a heartbeat update returns
fewer rows than expected, **the worker has lost that job** — it cancels the job's context
immediately rather than finishing work nobody will accept.

**Reaper.** A single Service loop, every 15 s, running **only on the leader** (§4.8 #6):

```sql
UPDATE jobs
   SET state = CASE WHEN attempt >= max_attempts THEN 'discarded' ELSE 'retryable' END,
       locked_by = NULL, locked_at = NULL, lease_expires_at = NULL,
       scheduled_at = now() + backoff(attempt),
       finalized_at = CASE WHEN attempt >= max_attempts THEN now() END,
       errors = errors || jsonb_build_object('at', now(), 'attempt', attempt,
                                             'worker', locked_by, 'err', 'lease expired')
 WHERE state = 'running' AND lease_expires_at < now()
RETURNING id, state;
```

**The stale-worker guard — the detail everyone forgets.** Every completion/failure write must assert
the claim it was made under:

```sql
UPDATE jobs SET state='succeeded', finalized_at=now()
 WHERE id=$1 AND state='running' AND locked_by=$2 AND attempt=$3;
```

Without `locked_by` + `attempt`, a zombie worker that comes back from a GC pause can mark a job
`succeeded` that the reaper already requeued and another worker is currently running — silently
losing the second attempt's result. One-line fix, and a great thing to be able to explain.
`0 rows affected` is not an error here; it is the signal that the claim was lost, and it gets a WARN
line plus a `job_claims_lost_total` increment.

**Clean shutdown skips all of this.** On SIGTERM: stop claiming → wait up to `shutdown_grace` for
in-flight jobs → for anything still running, explicitly release (`state='available'`,
`locked_by=NULL`, attempt decremented) so a rolling deploy doesn't leave jobs invisible for a full
lease period. Only *crashes* pay the lease timeout.

### §4.5 — Idempotency, at three layers

At-least-once is the only delivery guarantee a queue like this can honestly make. Exactly-once
*effects* are achievable; exactly-once *delivery* is not. Three layers:

**1. Producer / HTTP (`Idempotency-Key`).** Per `draft-ietf-httpapi-idempotency-key-header` (the
IETF WG draft, itself modelled on Stripe/PayPal). On `POST /tasks`:

- Client sends `Idempotency-Key: <uuid>`. We store `key`, `fingerprint = sha256(method|path|body)`,
  and `state='in_progress'` with `INSERT … ON CONFLICT DO NOTHING` — an insert that conflicts means a
  concurrent or prior request with that key.
- Same key + **same** fingerprint + `completed` → replay the stored status and body, add
  `Idempotency-Replayed: true`.
- Same key + same fingerprint + `in_progress` → `409 Conflict` (request still running).
- Same key + **different** fingerprint → `422 Unprocessable Entity` (key reuse — the draft's
  prescribed behaviour).
- Row + response are written **in the same transaction** as the task, so a replay can never return a
  response for a task that rolled back.
- 24 h TTL; a purge loop reaps expired keys.

**2. Enqueue dedup.** `jobs.unique_key` with a partial unique index over the live states means the
same logical event cannot be queued twice while one is in flight. `ON CONFLICT DO NOTHING`, and the
`jobs_enqueued_total{outcome="deduped"}` counter makes it visible. (River's `UniqueOpts{ByArgs:true}`
is the same index; §4.8 #12.)

**3. Consumer / effect.** Handlers must be idempotent, and we make that structural rather than
aspirational:
- For DB-effect jobs: the effect and the `state='succeeded'` write happen in **one transaction**, so
  "did work but crashed before ack" is impossible.
- For the webhook job (an external effect we can't transact with): we send
  `Idempotency-Key: <job_id>:<attempt>` and `X-Event-Id: <task_id>:<version>`, and document that the
  receiver must dedupe. That is the only honest answer for a remote side effect, and saying so out
  loud is better than pretending otherwise.

### §4.6 — Wake-up: LISTEN/NOTIFY + polling

Polling alone at 2 s costs 2 s of median latency. `NOTIFY` alone is unreliable — notifications are
fire-and-forget, dropped if nobody is listening, and lost across a reconnect. So: **both**.

- After a producing transaction **commits**, the app issues `pg_notify('jobs_available', kind)`.
  (Not from an `AFTER INSERT` trigger — a trigger's NOTIFY also only fires at commit, but doing it in
  app code keeps the behaviour visible in Go rather than hidden in DDL.)
- One **dedicated** connection (a standalone `pgx.Conn`, not from the pool) runs
  `LISTEN jobs_available` and pushes to a buffered channel. A pooled connection cannot safely hold a
  long-lived LISTEN.
- The claim loop selects on `{notify, poll ticker, ctx.Done()}`. The ticker is the safety net that
  turns best-effort NOTIFY into at-least-once pickup.
- **Fetch cooldown** (100 ms, river §4.8 #11): a minimum gap between claims after a NOTIFY. Without
  it, a burst of 1000 inserts triggers 1000 claim round-trips.
- On listener disconnect: reconnect with backoff, and **immediately do a full poll** — anything
  notified during the gap is picked up by the poll, not lost.
- Ceiling, stated honestly: comfortable into the low thousands of jobs/s. Past ~10 k/s you want a
  real broker — `LISTEN/NOTIFY` serialises on a global lock, and the state churn makes the table a
  vacuum problem before that.

### §4.7 — Connection health / "keepalive"

You called this out specifically. Concretely, in `pgxpool.Config`:

| Setting | Value | Why |
|---|---|---|
| `MaxConns` / `MinConns` | 20 / 2 (API), separate pool for worker | **bulkhead** — a saturated worker pool must not starve the API |
| `MaxConnLifetime` | 30 m | forces rotation past LB/proxy idle reaping and lets failover drain |
| `MaxConnIdleTime` | 5 m | releases idle capacity |
| `HealthCheckPeriod` | 30 s | pgx background ping evicts dead conns before a request finds them |
| `statement_timeout` | 5 s (session, via `RuntimeParams`) | a runaway query cannot pin a connection forever |
| `idle_in_transaction_session_timeout` | 10 s | a bug that leaves a tx open cannot block VACUUM indefinitely |
| `lock_timeout` | 3 s | migration and hot-path safety |
| TCP keepalive | 30 s idle / 10 s interval / 3 probes | detects silently-dead NAT'd connections (the LISTEN conn especially) |
| `connect_timeout` | 5 s | bounded startup failure |
| `application_name` | `taskapi-{role}-{instance}` | shows up in `pg_stat_activity` — free debuggability |

Plus: **every query takes a `context.Context` with a deadline**, retry only on
`pgconn.SafeToRetry(err)` (connection-level, pre-send failures), and never blind-retry a write
without idempotency. Readiness (`/readyz`) pings the pool; liveness (`/healthz`) does not — a DB
outage must not get your pods killed and restarted into a thundering herd.

### §4.8 — What we take from river (the census)

River is ~4 years of production lessons about this exact table. Building our own is the right call
for the *exercise*, but rediscovering its lessons by accident is not. Read this before writing queue
code; each row is a deliberate steal, with the failure it prevents.

**Adopt — high value, low cost:**

| # | River mechanism | The failure it prevents | Cost |
|---|---|---|---|
| 1 | **`retryable` as a state distinct from `available`** | a failed job is instantly re-claimable, hot-loops the pool, and starves fresh work | 1 enum value |
| 2 | **A separate `scheduler` loop** (`retryable`/`scheduled` → `available` when `scheduled_at` passes) | conflating "is it due" with "is it stuck" — two different queries, two different indexes, one of them hot | ~30 lines |
| 3 | **Terminal vs retryable errors** (`river.JobCancel`) — handler returns `queue.ErrTerminal` to skip remaining attempts | retrying a poison job (webhook 400, malformed payload) 5× is pure waste and hides the real bug. **The #1 thing hand-rolled queues get wrong** | an `errors.Is` check |
| 4 | **Snooze** (`river.JobSnooze`) — reschedule **without consuming an attempt** | backpressure is not failure. Breaker open (§5) → snooze, don't burn the attempt budget on a dependency outage and discard a valid job | 1 sentinel + 1 branch |
| 5 | **`ErrorHandler` with separate `HandleError` / `HandlePanic`** | a panic in one handler kills the worker goroutine and, unguarded, the pool. Both paths need a WARN line and a counter | ~20 lines |
| 6 | **Leader election for maintenance** — only one replica runs reaper/scheduler/purger | N replicas × the same `UPDATE … WHERE lease_expires_at < now()` = N-way write contention on identical rows | `pg_try_advisory_lock` per tick (~10 lines; river uses a `river_leader` table, overkill here) |
| 7 | **`errors jsonb[]` history + `attempted_by text[]`** | "it failed 4 times" with only the last error, and no idea which worker | 2 columns |
| 8 | **Retry backoff `attempt⁴ + jitter`** (≈1 s, 16 s, 81 s, 256 s, 625 s) | `2^n` is too aggressive early and too short at the tail for a dependency outage; no jitter → all replicas retry in lockstep | a 1-line function |
| 9 | **`JobTimeout` — per-attempt context deadline** | a handler with no deadline is precisely how a job gets stuck forever, and the reason the reaper exists at all | `context.WithTimeout` |
| 10 | **A completion event channel** (`client.Subscribe`) | integration tests that `time.Sleep(2*time.Second)` and flake in CI. Subscribe → insert → wait on the event. **Biggest single testing win** (§10.3) | a `chan JobEvent` |
| 11 | **`FetchCooldown`** — min gap between claims after NOTIFY | a 1000-row insert burst → 1000 claim round-trips | 1 rate limiter |
| 12 | **Unique jobs by args** (`UniqueOpts{ByArgs}`) | duplicate events | already in §4.5 layer 2 |

**Skip — deliberately, and say why in the README:**

| River feature | Why not here |
|---|---|
| Per-queue `MaxWorkers` map | one job kind. It *is* the right answer for isolating slow kinds from fast ones — cite it as the scale answer |
| Periodic jobs / `RunOnStart` | our maintenance loops are plain tickers in the `workers()` table (§9); cron-in-queue is a different problem |
| Middleware / hooks framework | one handler. A function wrapper is enough; a plugin system for a single caller is architecture theatre |
| Reindexer service | index bloat only becomes real at sustained high volume — note it in the scale paragraph |
| River UI | Grafana is our console (§6.5) |
| `river_client` fleet tracking | one to three replicas |

**Where we go beyond river:** lease + heartbeat (§4.4). River's static `RescueStuckJobsAfter` cannot
tell *slow* from *dead*; a heartbeat can. Worth one README paragraph, because "I read the leading
library, took these nine things, and here is the one place I disagreed with it and why" is a
substantially stronger answer than either "I used river" or "I wrote my own".

### §4.9 — The README paragraph this section owes

> **Why not `riverqueue/river`?** In production I would use it — it is the mature Go/Postgres queue
> and it solves this exact problem. Here I built the mechanism (`FOR UPDATE SKIP LOCKED`, lease +
> heartbeat, stale-worker guard, transactional outbox) because the mechanism is what the exercise is
> testing. `internal/queue/README.md` documents which of river's designs I adopted and which I
> declined — including the one place I went further than it does, per-job heartbeats, because a
> static rescue timeout cannot distinguish a slow job from a dead worker.

That paragraph converts "reinvented the wheel" into "evaluated the wheel". Without it, D4 is a risk.

### §4.10 — Housekeeping

- **Purge loop** (hourly, leader-only): delete `succeeded` older than 24 h, `cancelled`/`discarded`
  older than 7 d. A queue table that never deletes grows forever and takes the index with it.
- **Bloat:** covered by the per-table autovacuum settings above; `fillfactor=80` lets the
  `available → running → succeeded` updates be HOT (no index churn).
- **Scale note for the README:** partition `jobs` by `created_at` (monthly), or move finalized rows
  to `jobs_archive`, once volume justifies it.
- **Migrations:** `golang-migrate` (or `pressly/goose`), versioned SQL in `migrations/`, run by a
  one-shot compose service **and** guarded by `pg_advisory_lock` so concurrent replicas can't race.

---

## §5 — Rate limiting and circuit breaking

The framing that matters, and the thing to say in the interview: **a rate limiter protects you from
your callers; a circuit breaker protects you from your dependencies.** They point in opposite
directions and are not substitutes. Putting a breaker on an inbound handler, or a rate limiter on an
outbound call as a failure guard, are both common and both wrong.

### §5.1 — Where each one lives here

```
client ──►[ inflight limiter ]──►[ per-client rate limiter ]──► API ──► Postgres
                (load shed)             (fairness)

worker ──►[ timeout ]──►[ retry+backoff ]──►[ circuit breaker ]──►[ egress rate limiter ]──► webhook
```

### §5.2 — Inbound: two tiers

**Tier 1 — global in-flight limiter (load shedding).** A counting semaphore in middleware, default
200. This is what actually keeps the process alive under overload; a *rate* limiter does not, because
200 rps of 10-second requests is still 2000 concurrent goroutines. Over the limit → `503` with
`Retry-After`. (`golang.org/x/net/netutil.LimitListener` is the connection-level alternative; the
middleware is better because it can return a proper response.)

**Tier 2 — per-client rate limiter (fairness).** Keyed on `client_id` from the bearer token
(§5.2a), falling back to client IP for anonymous callers.

Algorithm: **token bucket** via `golang.org/x/time/rate`, one `*rate.Limiter` per key in a
TTL-evicting map (a plain map is an unbounded memory leak keyed by attacker input — this is the bug
in most blog-post implementations). Key TTL 10 m, swept every 1 m.

**Quota by tier**, not one global number — which is what makes the limiter worth having at all:

| Tier | Rate | Burst | Who |
|---|---|---|---|
| `anonymous` | 10/s | 20 | no token; keyed by IP |
| `standard` | 100/s | 200 | a configured client |
| `internal` | 1000/s | 2000 | our own worker / health probes |

### §5.2a — Client identity: the limiter's key (D8)

A static bearer token, resolved to a `client_id` and a tier. It exists to give the limiter and the
dashboards a real tenant dimension; it is not a general auth system, and the README says so.

**`auth.mode` has three values, and the default is the important one:**

| Mode | Behaviour | Used by |
|---|---|---|
| **`optional`** (default) | valid token → that client's tier. No token → `anonymous` tier, keyed by IP. **Never 401s.** | `go run`, the reviewer's first curl |
| `required` | no/invalid token → `401` + `WWW-Authenticate: Bearer realm="taskapi"` | the compose "prod-ish" profile |
| `off` | everything is `anonymous` | benchmarks |

`optional` is the default for exactly the reason `--storage=memory` is (§0): a reviewer running
`curl -X POST localhost:8080/api/v1/tasks` and getting `401` concludes the exercise is broken. An
anonymous tier costs nothing and turns auth from a gate into a *quota dimension*, which is what real
APIs actually do.

**Implementation notes that matter:**

- Tokens are stored as **SHA-256 hashes** in config; the plaintext lives only in env / `.env` /
  a secret file. `.env.example` ships a dev token so the quickstart works.
- Comparison is `crypto/subtle.ConstantTimeCompare` on the hash — a plain `==` on a secret is a
  timing oracle, and it's the kind of thing a security-minded reviewer greps for.
- The token is **never logged at any level** (§2.3). Log `client_id`; if you must log the token for
  debugging, log the first 8 hex of its hash.
- **Cardinality rule (§6.1):** `client_id` is safe as a metric label *only* because it comes from a
  bounded config list. The fallback value is the literal string `anonymous` — **never the IP**.
  An IP label is an unbounded, attacker-controlled cardinality bomb.
- IP extraction trusts exactly `http.trusted_proxy_hops` entries from the right of
  `X-Forwarded-For`. Trusting the leftmost value is attacker-controlled and makes the limiter
  decorative.
- `401` maps to connect `CodeUnauthenticated` (§1.6). There are no scopes, so no `403`.

*Why token bucket over sliding-window/GCRA:* burst tolerance is the right behaviour for an API, it's
O(1) with no history, and it's in the standard extended library. GCRA (`throttled/v2`) is the
smoother choice and worth one README sentence.

**Response contract** — this is where "industry standard" has actually converged:

```
HTTP/1.1 429 Too Many Requests
Retry-After: 1
RateLimit: limit=100, remaining=0, reset=1          ← draft-ietf-httpapi-ratelimit-headers-11
RateLimit-Policy: 100;w=1                           ← the policy being applied
RateLimit-Limit: 100                                ← de-facto legacy trio, still what clients read
RateLimit-Remaining: 0
RateLimit-Reset: 1
```

The IETF draft (currently `-11`, May 2026, still a draft not an RFC) collapsed to two fields
`RateLimit` + `RateLimit-Policy`; the older `RateLimit-Limit/Remaining/Reset` trio is what GitHub,
Twitter, and most SDKs still consume. Emit both, document why, and put it in the OpenAPI. Successful
responses carry the headers too, so clients can self-pace rather than discovering the wall.

**Honest caveat for the README:** an in-process limiter is per-replica. Three replicas behind an LB
means 3× the nominal limit. Real deployments enforce this at the edge (Envoy/nginx/APISIX/Cloudflare)
with a shared Redis counter; the in-process one is a backstop that survives the edge being
misconfigured. Saying this unprompted is the difference between "used a library" and "understands
the control plane".

### §5.3 — Circuit breaker: settings that are defensible

Applied **per outbound target** (keyed by host — never one global breaker, or a slow analytics
endpoint trips the payment one).

| Knob | Value | Why this value |
|---|---|---|
| Failure-rate threshold | **50 %** | the near-universal default (Resilience4j, Envoy, Polly). Below ~50 % you trip on normal noise |
| Minimum throughput | **20 requests** in the window | without a floor, 1 failure out of 1 request opens the circuit. The single most common misconfiguration |
| Window | **10 s**, sliced into **10 buckets** of 1 s | time-based rolling window; bucketed so old data ages out smoothly instead of falling off a cliff |
| Open duration | **30 s** ± jitter | long enough for a restart/failover; jitter so all replicas don't probe in lockstep |
| Half-open trial calls | **5** concurrent max | probe without re-flooding a recovering dependency |
| Half-open → closed | **3 of 5 successes** | one lucky success is not recovery |
| Half-open → open | any failure | fail fast back to open |
| Per-attempt timeout | **3 s** | a breaker with no timeout never trips — the calls just hang |

**What counts as a failure** is the other half of the configuration and is usually left wrong:

- ✅ count: connection errors, timeouts, `5xx`, `429` from the dependency
- ❌ do **not** count: `4xx` other than 429 (that's *our* bug, not their outage — counting them means
  a validation bug takes down a healthy dependency)

**Compose with, not instead of:**
- **Timeout** per attempt (above) — a breaker without one is inert.
- **Retry**: max 3 attempts, exponential backoff base 200 ms cap 5 s, **full jitter**
  (`sleep = rand(0, min(cap, base*2^n))` — AWS's recommendation; equal jitter and no jitter both
  produce retry convergence). Retries happen **inside** the breaker's accounting so a retry storm
  actually trips it.
- **Retry budget**: cap retries at ~10 % of request volume, so a total outage costs 1.1× traffic, not
  4×. This is the Google SRE / Envoy answer to retry amplification.
- **Bulkhead**: bounded worker pool + a separate pgx pool per role (§4.7). Failure containment by
  resource partition.
- **Fallback → snooze, not fail** (§4.8 #4): when the breaker is `open`, the handler returns
  `queue.ErrSnooze(openDuration + jitter)`, which reschedules the job **without consuming an
  attempt**. This pairing is the whole point of having both: a dependency outage must not exhaust a
  valid job's retry budget and discard it to the DLQ. The breaker converts a slow failure into a
  fast one; snooze stops that fast failure from being counted as the job's fault.

### §5.4 — About go-micro's wrappers

`go-micro/plugins/v5/wrapper/{breaker,ratelimiter}` do exist. Two reasons they don't help here: they
wrap the **go-micro RPC** client/server chain, not `net/http`, so they're bypassed the moment we
serve the required REST paths (§1.1); and the historic hystrix breaker plugin is built on
`afex/hystrix-go`, which has been effectively unmaintained for years — Netflix itself put Hystrix in
maintenance mode in favour of adaptive concurrency limits. Not what you want to point at in an
interview.

### §5.5 — Library choice (D5)

**`failsafe-go/failsafe-go`** — recommended. It composes retry + timeout + circuit breaker +
bulkhead + hedging as an ordered policy chain, which is precisely where hand-rolled code goes wrong
(retry outside vs inside the breaker changes the semantics completely). Supports both count-based
(`WithFailureThresholdRatio(5, 10)`) and time-based
(`WithFailureRateThreshold(0.5, 20, 10*time.Second)`) thresholding, and half-open success thresholds.

**`sony/gobreaker`** — the alternative. Tiny, famous, ~300 lines, trivially explainable
(`ReadyToTrip: counts.Requests >= 20 && failureRatio >= 0.5`). If the goal is "the reviewer can read
the whole thing in 60 seconds", this wins. Retry/timeout would then be hand-composed.

I lean failsafe-go; tell me if you'd rather have the smaller surface.

- Package: `internal/resilience` (Library archetype) — constructs the policy chains, exposes them as
  a typed `Executor`, and registers the metrics in §6.4. Breaker state changes are also a `WARN`
  log line (§2.2) — a breaker that opens silently is a breaker nobody knows about.

---

## §6 — Observability (Prometheus + Grafana)

### §6.1 — Principles

- **RED for the API** (Rate, Errors, Duration) — the request-driven view.
- **USE for resources** (Utilisation, Saturation, Errors) — pools, workers.
- **Queue health has its own vocabulary**, and the single most important signal is not throughput —
  it's **oldest-pending age** (§6.5).
- **Naming**: Prometheus conventions — `_total` suffix on counters, base units (`seconds`, `bytes`),
  `snake_case`, one metric per *thing* with labels for dimensions.
- **Cardinality is a production incident waiting to happen.** Hard rules: `http_route` is the
  *templated* path (`/tasks/{id}`), **never** the raw URL. No user ids, task ids, IPs, or error
  strings as labels. Status is the numeric code, not the message. Every label set is bounded and I
  can name its bound.
- **Two listeners.** `:8080` public (API only). `:9090` admin (`/metrics`, `/healthz`, `/readyz`,
  `/debug/config`, `/debug/log-level`, `/debug/pprof/*`). Never expose pprof or your metrics on the
  public port. Lachesis does the same split.

### §6.2 — HTTP server (RED)

```
http_server_requests_total{method,route,status}                  counter
http_server_request_duration_seconds{method,route,status}        histogram
http_server_active_requests{method,route}                        gauge
http_server_request_body_size_bytes{method,route}                histogram
http_server_response_body_size_bytes{method,route}               histogram
```

**Buckets** — the OpenTelemetry HTTP semantic-convention default, which is the closest thing to an
industry standard and is what OTel-native backends assume:

```
0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10   (seconds)
```

15 buckets × ~24 label combinations (4 routes × ~6 status codes) ≈ 360 series. Bounded and explainable.

> **Native histograms** (stable in Prometheus 3.8) are the modern answer — no bucket choice, ~10×
> cheaper, `histogram_quantile` works at any resolution. Plan: expose **both** (the Go client
> supports `NativeHistogramBucketFactor: 1.1` alongside classic buckets on one metric), pin
> Prometheus ≥3.8 in compose, and note the migration in the README. Costs nothing, reads as current.

Derived queries the dashboard uses:
```promql
# Rate
sum by (route) (rate(http_server_requests_total[5m]))
# Errors (ratio)
sum(rate(http_server_requests_total{status=~"5.."}[5m])) / sum(rate(http_server_requests_total[5m]))
# Duration p50/p95/p99
histogram_quantile(0.99, sum by (le,route) (rate(http_server_request_duration_seconds_bucket[5m])))
```

### §6.3 — Queue (the section that proves the system works)

```
job_queue_depth{kind,state}                       gauge     ← pending backlog
job_queue_oldest_pending_age_seconds{kind}        gauge     ← THE health signal
job_wait_duration_seconds{kind}                   histogram ← enqueue → claim
job_processing_duration_seconds{kind,result}      histogram ← claim → finalize
jobs_processed_total{kind,result}                 counter   ← succeeded|failed|discarded
jobs_enqueued_total{kind,outcome}                 counter   ← inserted|deduped
jobs_retried_total{kind}                          counter
job_leases_expired_total{kind}                    counter   ← reaper reclaims = crashed/stuck workers
job_claim_batch_size                              histogram ← claimed vs requested → contention
job_workers_active{}                              gauge
job_workers_configured{}                          gauge
dead_letter_jobs_total{kind}                      counter
```

`job_processing_duration_seconds` gets **wider buckets** than HTTP — jobs are allowed to be slow:
`0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300`.

**Why oldest-pending-age is the one to alert on:** depth alone is ambiguous (10 000 jobs that drain
in 20 s is fine; 5 jobs stuck for an hour is an outage). Age is the direct measure of "is the queue
actually being served", and it catches the failure modes depth misses — a poison job blocking a
partition, all workers wedged on a hung dependency, a deploy that forgot to start the worker.

```promql
# alert: queue not being served
job_queue_oldest_pending_age_seconds > 300
# alert: retries are eating the queue
rate(jobs_retried_total[5m]) / rate(jobs_processed_total[5m]) > 0.25
# alert: workers are dying
rate(job_leases_expired_total[15m]) > 0
```

Depth and age come from a `prometheus.Collector` that runs one cheap query on scrape (lachesis'
`metrics.Collector` pattern) — **not** a background goroutine writing gauges, which drifts and lies
after a scrape gap.

### §6.4 — Resilience and dependencies

```
ratelimit_decisions_total{scope="global|client",tier,decision="allowed|throttled"}  counter
ratelimit_active_keys{tier}                                                        gauge
http_server_inflight_rejected_total{}                                              counter
auth_decisions_total{result="authenticated|anonymous|rejected"}                    counter
```

`tier` is bounded (3 values) and `client_id` is bounded by the config list — see the cardinality
rule in §5.2a. `client_id` goes on `http_server_requests_total` only; **never** an IP, anywhere.

```

circuit_breaker_state{name}                        gauge    0=closed 1=half_open 2=open
circuit_breaker_transitions_total{name,from,to}    counter
circuit_breaker_calls_total{name,result}           counter  success|failure|rejected|timeout

dependency_request_duration_seconds{target,result} histogram   outbound HTTP
dependency_retries_total{target}                   counter

db_pool_connections{pool,state="acquired|idle|constructing|max"}  gauge
db_pool_acquire_duration_seconds{pool}                            histogram
db_pool_acquires_total{pool,result="ok|empty|canceled"}           counter
db_query_duration_seconds{op}                                     histogram   ← op is a bounded enum
db_errors_total{op,class}                                         counter

config_reloads_total{result="applied|invalid|read_error"}         counter
build_info{version,commit,go_version,build_date}                  gauge = 1
```

Plus `collectors.NewGoCollector(WithGoCollectorRuntimeMetrics(...))` for the modern
`runtime/metrics`-backed `go_*` series (GC pause distributions, sched latency) and
`NewProcessCollector` for `process_*`.

### §6.5 — Dashboards (provisioned as code)

Four JSON dashboards in `deploy/grafana/dashboards/`, wired by a provisioning file so they exist on
first `docker compose up` — a dashboard you have to import by hand is a dashboard the reviewer never
sees.

1. **API — RED.** Rate/error-ratio/latency-quantiles per route, status-code heatmap, in-flight,
   429/503 shed rate.
2. **Queue health.** Depth by state, **oldest-pending age**, wait vs processing latency, throughput
   by result, retry ratio, lease expirations, worker utilisation, DLQ count.
3. **Resilience.** Breaker state timeline (a state-timeline panel — instantly readable), transitions,
   rejected calls, dependency latency, limiter allow/throttle, retry rate.
4. **Runtime & DB.** Goroutines, heap, GC, pool utilisation & acquire wait, query latency by op.

### §6.6 — Alerts (SLO-based, with the standard numbers)

`deploy/prometheus/rules.yml`. Availability SLO **99.9 %** over 30 days (43 m 12 s error budget),
latency SLO **p99 < 500 ms**. Multi-window multi-burn-rate alerting, straight out of the Google SRE
Workbook — these are the "common values" you asked for:

| Severity | Burn rate | Long window | Short window | Budget consumed | Meaning |
|---|---|---|---|---|---|
| page | **14.4×** | 1 h | 5 m | 2 % in 1 h | budget gone in ~2 days |
| page | **6×** | 6 h | 30 m | 5 % in 6 h | budget gone in ~5 days |
| ticket | **3×** | 1 d | 2 h | 10 % in 1 d | |
| ticket | **1×** | 3 d | 6 h | 10 % in 3 d | |

The short window is always long-window/12 and both must fire — that's what kills the flapping and
the "alert fires 6 hours after the incident ended" problem of single-window alerts.

Plus the non-SLO operational alerts from §6.3, `circuit_breaker_state == 2 for 5m`, and
`up == 0`.

### §6.7 — Logs / traces / metrics correlation (D6: stretch)

Wire the plumbing now even if the collector is a stretch goal: `otelhttp` middleware and `otelpgx`
tracer generate `trace_id`/`span_id`; the context logger emits them on every record (§2.3); a span
per job with `job.id`/`job.kind`/`job.attempt` attributes. If we ship the collector, add
**exemplars** on the latency histograms (Go client supports it; Prometheus needs
`--enable-feature=exemplar-storage`) so a Grafana latency spike is one click from the trace that
caused it. That's the demo that lands.

---

## §7 — OpenAPI from protobuf

Answered inline in §1.2 — **yes, protobuf solves exactly the problem you described**, and the path
convention survives. Restating the pipeline as the deliverable:

### §7.1 — Toolchain

`buf.yaml` + `buf.gen.yaml`, with `buf.lock` for `googleapis` and `protovalidate` deps.

| Plugin | Output |
|---|---|
| `buf.build/protocolbuffers/go` | `gen/task/v1/*.pb.go` — the structs |
| `buf.build/connectrpc/go` | `gen/task/v1/taskv1connect/*.connect.go` — handler + client |
| `github.com/sudorandom/protoc-gen-connect-openapi` | `docs/openapi.yaml` — **OpenAPI 3.1** |

`protoc-gen-connect-openapi` understands the `google.api.http` annotations (its `grpcgateway` mode)
*and* `protovalidate`/`buf.validate` constraints *and* gnostic OpenAPI annotations — so
`status` being an enum of `[0,1]` and `name` being 1–255 chars are declared once in the proto and
appear in the generated schema **and** are enforced at runtime by the validation interceptor. That
is the single-source-of-truth property, fully realised.

Generated code **is committed** to the repo — the reviewer must be able to `go build` without
installing buf.

### §7.2 — Validation

`protovalidate-go` interceptor, rules in the proto:

```proto
message Task {
  string id     = 1 [(buf.validate.field).string.uuid = true];
  string name   = 2 [(buf.validate.field).string = {min_len: 1, max_len: 255}];
  int32  status = 3 [(buf.validate.field).int32 = {in: [0, 1]}];
}
```

One declaration → the OpenAPI constraint, the runtime check, and the 400 response. No hand-written
validation code, no drift, and the error message names the field.

### §7.3 — Serving the docs

- **Security scheme declared in the spec**, so the "Try it" console has an auth box:
  `securitySchemes: {bearerAuth: {type: http, scheme: bearer}}`, applied as an *optional* security
  requirement (`security: [{}, {bearerAuth: []}]` — the empty object is what says "anonymous is
  allowed", matching `auth.mode: optional` in §5.2a). Declared via gnostic annotations on the proto
  or `protoc-gen-connect-openapi`'s config, not hand-patched into the YAML.
- `GET /openapi.yaml` and `/openapi.json` on the **public** listener (embedded via `go:embed`, so the
  binary is self-contained).
- `GET /docs` → **Scalar** or Swagger UI, single HTML page pointing at the spec. A reviewer clicking
  one link and getting a working "Try it" console is worth a lot of goodwill.
- CI check: regenerate and `git diff --exit-code` — the committed spec **cannot** drift (§10.4).
- `buf lint` + `buf breaking --against '.git#branch=main'` in CI.

### §7.4 — API versioning (DECIDED — D1b)

**Canonical surface is `/api/v1/…`.** An unversioned public API is a one-way door: the first
breaking change either breaks every client or forces a parallel deployment. Path versioning is the
pragmatic industry default (Stripe's date-versioning and media-type versioning are the alternatives;
both cost more than they return at this scale).

The version already exists in three places and they must agree:

| Layer | Value |
|---|---|
| proto package | `task.v1` |
| Connect/gRPC native route | `/task.v1.TaskService/ListTasks` |
| REST route | `/api/v1/tasks` |

**Conflict with the PDF, and how it's resolved.** The exercise specifies bare `/tasks`, and
"endpoints should work as expected" is requirement #1 — a reviewer curling `POST /tasks` and getting
404 fails the exercise before reading any code. So both are bound:

- `/api/v1/tasks*` — canonical, documented, what the README's examples use.
- `/tasks*` — same handler via `additional_bindings`, returns 200, and is marked
  `deprecated: true` in the generated OpenAPI. Responses on the legacy path carry
  `Deprecation: true` and `Sunset: <date>` (RFC 8594 / RFC 9745) plus a `Link: rel="successor-version"`
  pointing at the v1 path.
- Rejected: a `308 Permanent Redirect` from the bare paths. 308 preserves method and body correctly,
  but `curl` without `-L` returns 308 and a checklist-driven reviewer reads that as broken.

**Unversioned by design** (infrastructure, not API contract): `/healthz`, `/readyz`, `/metrics`,
`/openapi.yaml`, `/openapi.json`, `/docs`, `/debug/*`, `/version`.

**Metric cardinality note for §6.1:** the `route` label now has 8 values instead of 4. Still bounded,
still templated (`/api/v1/tasks/{id}`, never the raw id). The legacy paths keep their own label
values, which is a free deprecation-usage dashboard — you can see whether anyone still calls them.

**README line to write:** *"v2 would be added as a second proto package `task.v2` with its own
`/api/v2` bindings, served by the same binary, sharing the domain service. v1 stays until the
`Sunset` date."* That sentence is what the versioning question is actually testing.

### §7.5 — Other contract details worth deciding

- **`PUT /tasks/{id}` is a full replace** (per HTTP semantics). Body carries `name` + `status`; both
  required. If you want partial update, that's `PATCH` with a `FieldMask` — out of the spec's scope,
  mention in README.
- **`GET /tasks`** supports `?status=0|1` filter and **keyset pagination** (`?limit=&page_token=`)
  returning `next_page_token`. Offset pagination degrades on large tables; keyset is the correct
  default and it's one index (`tasks_created_at_id_idx`). Default limit 20, max 100.
- **Response envelope:** the exercise doesn't specify one. I'll return
  `{"result": [...], "next_page_token": "..."}` for list and the bare object for single-item, and say
  so in the README. (Flag if you'd rather match a specific house style.)
- `DELETE` returns **204 No Content**; deleting an already-deleted id returns 404 (not idempotent-200
  — the spec is silent and 404 is more informative; one README line either way).

---

## §8 — Docker and the local stack

### §8.1 — Dockerfile

Multi-stage, distroless, reproducible:

```dockerfile
# syntax=docker/dockerfile:1
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev COMMIT=none DATE=unknown
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
      -o /out/taskapi ./cmd/taskapi

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/taskapi /taskapi
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/taskapi"]
CMD ["all"]
```

`CGO_ENABLED=0` + `distroless/static` → ~15 MB image, no shell, no package manager, non-root by
default. `-trimpath` for reproducibility. `.dockerignore` excluding `.git`, `deploy/`, `*.md`.
Version metadata is injected at build time and surfaces in `build_info` (§6.4) and `/version`.

> Note: distroless has no shell, so `HEALTHCHECK` can't use `curl`. Either add a `taskapi healthcheck`
> subcommand that hits `/readyz` (self-contained, the clean answer), or let compose's
> `healthcheck: test: ["CMD", "/taskapi", "healthcheck"]` do it. I'll do the subcommand.

### §8.2 — compose

```
services:
  postgres      postgres:17-alpine  · healthcheck pg_isready · named volume · tuned shared_buffers
  migrate       taskapi image · command: migrate up · depends_on postgres(healthy) · restart: no
  api           taskapi image · command: serve  · depends_on migrate(completed_successfully)
  worker        taskapi image · command: worker · depends_on migrate(completed_successfully)
  webhook-sink  tiny Go/httpbin echo · env-controlled failure injection (§8.3)
  prometheus    prom/prometheus:v3.x · scrapes api:9090 + worker:9090 · rules.yml mounted
  grafana       grafana/grafana · provisioned datasource + 4 dashboards · anonymous admin viewer
  [optional]    jaeger or grafana/tempo (D6)
```

Key details:
- `depends_on: { condition: service_healthy }` for postgres, `service_completed_successfully` for
  migrate — so nothing races the schema.
- `api` and `worker` run the **same image**, different subcommand. That's the decoupling proof.
- Grafana provisioned with anonymous access enabled (`GF_AUTH_ANONYMOUS_ENABLED=true`,
  `GF_AUTH_ANONYMOUS_ORG_ROLE=Viewer`) so the reviewer hits `localhost:3000` and sees dashboards
  with zero clicks. Login screens lose readers.
- `.env.example` committed; `.env` gitignored.
- Resource limits on every service so `compose up` can't eat the reviewer's laptop.

### §8.3 — The demo, and why `webhook-sink` earns its place

A 40-line service that echoes and can be told to misbehave:
`WEBHOOK_FAIL_RATE=0.8`, `WEBHOOK_LATENCY=5s`, or `POST /control {mode: down}`.

That gives us reproducible, one-command demos of every claim in this plan:

| `make` target | What the reviewer watches |
|---|---|
| `make demo-load` | k6/vegeta ramp, **two lanes** — anonymous (10/s) throttles hard, `Bearer $DEMO_TOKEN` (100/s) sails through. RED dashboard fills, `RateLimit` headers show the differing quotas |
| `make demo-breaker` | sink → `down` → breaker timeline goes closed→open→half_open→closed, jobs retry with backoff instead of failing |
| `make demo-crash` | `docker kill worker` mid-job → `job_leases_expired_total` increments, reaper requeues, another worker completes it, **exactly once** |
| `make demo-idempotency` | same `Idempotency-Key` twice → one task, identical response, `Idempotency-Replayed: true` |

Each is one command and each corresponds to a paragraph in the README. This is what converts a wall
of architecture prose into something a reviewer *believes*.

### §8.4 — Makefile / CI

`make generate` (buf) · `test` · `test-integration` (testcontainers) · `lint` (golangci-lint) ·
`build` · `up` / `down` · `demo-*`.

GitHub Actions: `go vet` → `golangci-lint` → `go test -race -cover ./...` → integration tests →
`buf lint` + `buf breaking` → **generate-and-diff check** (§7.3) → docker build. Badge in the README.

---

## §9 — Package layout

Reusing the lachesis four-archetype model, because it's a convention you already defend and it makes
every package's shape predictable before you open it:

> **Service** — owns a long-running loop; constructor → struct, blocking `Run(ctx) error`, cheap
> health accessors, **never spawns its own goroutines** (only the composition root's `workers()`
> table starts one). **Store** — passive shared state, bare `New()`, explicit lock discipline, no
> ctx/goroutines/IO. **Driven** — one owner sequences verb methods, everyone else gets lock-free
> read accessors. **Library** — stateless functions, no lifecycle.

```
cmd/taskapi/                  cobra root: serve | worker | all | migrate | healthcheck | version
proto/task/v1/task.proto      THE source of truth
gen/                          buf output (committed)
migrations/                   golang-migrate SQL
deploy/{prometheus,grafana}/  provisioning + dashboards + rules
docs/openapi.yaml             generated, committed, CI-diffed
internal/
  app/          ← composition root (sanctioned one-off). Wires everything; owns workers() in drain order
  config/       Library   — struct, koanf load, Validate(), hot-reload diff
  logging/      Library   — slog setup, LevelVar, LevelTrace, TTY/JSON handlers, redaction
  observability/Driven    — prom registry, metric structs, OTel setup, custom collectors
  task/         Library+  — domain: Task, Service (protocol-agnostic), sentinel errors, Repository iface
  task/memrepo/ Store     — in-memory repository (the PDF's literal requirement)
  task/pgrepo/  Library   — pgx repository + outbox writer, one tx
  queue/        Service   — Claimer loop, Heartbeater, Reaper, Purger, WorkerPool
  queue/handler/Library   — job handlers (task.event → webhook)
  outbox/       Library   — enqueue helpers, NOTIFY on commit
  resilience/   Library   — failsafe policy chains, breaker registry + metrics
  httpx/        Library   — middleware: recover, requestid, logger, metrics, ratelimit, timeout, maxbody
  auth/         Store     — bearer token → client_id + tier; constant-time compare (§5.2a)
  idempotency/  Library   — Idempotency-Key store + middleware
  api/          Library   — connect service impl; proto ⇄ domain adapter; error mapping
  admin/        Driven    — /healthz /readyz /metrics /debug/{config,log-level,pprof}
  testenv/                — shared test infra (testcontainers, fixtures, golden helpers)
```

Two conventions carried over verbatim:

- **Interfaces are consumer-defined only.** `task.Repository` is declared where it's *used*, with the
  minimal method set, and only because a second implementation genuinely exists (memory + postgres).
  No speculative producer-side interfaces.
- **`app.workers()` is a single table listing every long-lived goroutine in drain order**, and the
  shutdown test is **goleak-enforced**. This is the thing that makes graceful shutdown provable
  rather than hopeful.

---

## §10 — Testing

The PDF asks for "unit tests". What actually demonstrates seniority is testing the things that are
*hard* to test — concurrency, crash recovery, and contract drift.

### §10.1 — Unit (fast, no Docker)

Table-driven, `t.Parallel()`, on: `task.Service` against `memrepo` (all CRUD paths, validation,
not-found, optimistic-lock conflict) · config precedence (the four-layer merge, one case per layer
overriding the one below) · `envToPath` mapping · backoff-with-jitter bounds · limiter key extraction
from `X-Forwarded-For` with a spoofed header · error → connect-code mapping · redaction
(`ReplaceAttr` never emits a DSN password **or a bearer token**).

Auth specifically (§5.2a): `mode: optional` + no token ⇒ 200 at the anonymous tier · `mode: required`
+ no token ⇒ 401 with `WWW-Authenticate` · unknown token ⇒ anonymous (optional) / 401 (required) ·
valid token ⇒ that client's tier applied · token comparison is constant-time · a spoofed
`X-Forwarded-For` cannot change the limiter key when `trusted_proxy_hops: 0`.

### §10.2 — HTTP contract

`httptest.Server` over the **real vanguard mux**, asserting the exercise's exact contract: the four
methods on the exact paths, status codes, JSON shape, `status` enum rejection of `2`, 404 on unknown
id, 400 on malformed body, 405 on wrong method, 413 on oversized body. This is the test that proves
the requirement is met — it should be the first one a reviewer opens, so it goes in
`internal/api/contract_test.go` with a header comment pointing at the PDF.

**Run every case twice, table-driven over `prefix = {"/api/v1", ""}`** (§7.4). Both surfaces must be
byte-identical in status and body; only the legacy one carries `Deprecation`/`Sunset` headers. This
is the test that stops a future refactor from quietly 404-ing the paths the PDF asked for.

### §10.3 — Integration (testcontainers-go, real Postgres)

Queue semantics **cannot** be faked — `SKIP LOCKED` behaviour, lease expiry, and transaction
visibility are properties of the database, not of our code. Real container, `t.Cleanup` teardown,
migrations applied per-suite, `-short` skips them.

**No `time.Sleep` anywhere in this suite.** Every test waits on the completion event channel
(§4.8 #10) with a context deadline. Sleeping for "long enough" is how queue tests become the flaky
ones everybody skips, and a reviewer who greps for `time.Sleep` in a concurrency test suite and
finds none has learned something about you.

| Test | What it proves |
|---|---|
| **N workers, M jobs** (16 × 1000) | every job runs **exactly once**; no double-claim. The core correctness claim |
| **Lease expiry** | worker claims, stops heartbeating, reaper requeues after lease, second worker completes |
| **Stale-worker guard** | zombie tries to ack after requeue → `0 rows affected`, does not corrupt the re-run |
| **Transactional outbox** | task insert rolled back ⇒ **no** job row. Task committed ⇒ job row exists. Both directions |
| **Enqueue dedup** | same `unique_key` twice while pending ⇒ one row |
| **Idempotency-Key** | replay returns identical response; different body with same key ⇒ 422 |
| **NOTIFY + poll** | kill the listener connection, insert a job, assert it's still picked up (by poll) |
| **Graceful shutdown** | SIGTERM mid-job ⇒ job finishes or is explicitly released to `pending`; never lost; goleak clean |
| **Backoff schedule** | attempt N's `scheduled_at` lands in the jittered window for N |
| **Terminal error** (§4.8 #3) | handler returns `ErrTerminal` ⇒ state `cancelled` on attempt 1, **not** 5 retries |
| **Snooze on open breaker** (§4.8 #4) | breaker `open` ⇒ job rescheduled with `attempt` **unchanged**; budget not burned |
| **Panic in handler** (§4.8 #5) | panic ⇒ job goes `retryable`, pool survives, `HandlePanic` fires, other workers unaffected |
| **Leader-only maintenance** (§4.8 #6) | 3 clients running ⇒ exactly one reaper sweep per tick, not three |

### §10.4 — Contract / drift

- `make generate && git diff --exit-code` in CI — the committed `gen/` and `docs/openapi.yaml`
  cannot drift from the proto.
- `buf lint`, `buf breaking --against main`.
- Golden test: the generated OpenAPI contains exactly the four required paths with the four required
  methods. If someone renames an RPC and forgets the annotation, this fails.

### §10.5 — Load / resilience (not in CI; `make demo-*`)

k6 or vegeta scripts under `test/load/`, driving the §8.3 demos. Their value is producing the
dashboard screenshots that go in the README.

**Coverage target:** ~80 % on `internal/task`, `internal/queue`, `internal/config`; don't chase a
global number — say so in the README rather than padding with tests of generated code.

---

## §11 — Build order

Each step leaves the repo in a working, committable state. If time runs out, we ship at any boundary
and the README's "Beyond the requirements" section shrinks accordingly.

| # | Milestone | Leaves you with |
|---|---|---|
| **0** | Repo skeleton, go.mod, Makefile, CI, cobra root, `internal/app` + `workers()` | `taskapi version` runs |
| **1** | **§7** proto + buf + generated code + OpenAPI + `/docs` | the contract exists, on paper |
| **2** | **§2 §3** logging tiers + config precedence + `--print-config` + SIGHUP | operable shell |
| **3** | **§1** vanguard mux + `task.Service` + `memrepo` + §10.2 contract test | **⭐ the PDF is fully satisfied here** — everything after is bonus |
| **4** | **§8** Dockerfile + minimal compose (api only) | reviewer can `docker run` |
| **5** | **§4a** Postgres repo, migrations, tx outbox, `--storage=postgres` | durable |
| **6** | **§4b** queue: claim / lease / heartbeat / reaper / purge + worker pool | the hard part |
| **7** | **§4c** idempotency (all three layers) | correct under retry |
| **8** | **§5** limiter + breaker + retry/backoff/bulkhead | resilient |
| **9** | **§6** metrics, admin listener, health/ready, dashboards, alert rules | observable |
| **10** | **§8** full compose + webhook-sink + `demo-*` targets | demonstrable |
| **11** | README, architecture diagram, dashboard screenshots, ADR notes | **the thing that actually gets read** |
| **12** | *(stretch, D6)* OTel traces + exemplars | |

**Milestone 3 is the checkpoint that matters.** Nothing after it may break it.

### §11.1 — The README is a deliverable, not an afterthought

Structure: 3-line quickstart → the four endpoints with `curl` examples → "Beyond the requirements —
and why" (one paragraph per section, each linking to the code and the demo command) → architecture
diagram → **"Decisions and trade-offs"** (why not go-micro, why Postgres over in-memory, why roll the
queue, at-least-once vs exactly-once, what I'd change at 100× scale) → dashboard screenshots.

That trade-offs section is, realistically, the highest-value 500 words in the whole submission. An
interviewer can skim 8000 lines of Go in four minutes; they will read your reasoning in full.

---

## §12 — References

Postgres queue mechanics —
[Prisma: Postgres job queue with SKIP LOCKED](https://www.prisma.io/blog/you-dont-need-a-job-queue-postgres-already-has-skip-locked) ·
[River (Go+Postgres queue)](https://riverqueue.com/) ·
[brandur.org: River design](https://brandur.org/river) ·
[River godoc — the §4.8 census source](https://pkg.go.dev/github.com/riverqueue/river) ·
[River: hooks & middleware design](https://riverqueue.com/blog/designing-hooks-and-middleware) ·
[Replacing your message queue with Postgres](https://mvpfactory.io/blog/replacing-your-message-queue-with-postgresql-skip-locked-queues-listen-notify) ·
[Transactional outbox in Go + Postgres](https://www.freecodecamp.org/news/how-to-implement-the-outbox-pattern-in-go-and-postgresql/) ·
[The transactional outbox is a ledger, not a queue](https://tiarebalbi.com/en/blog/the-transactional-outbox-is-not-a-queue)

Idempotency & rate-limit standards —
[draft-ietf-httpapi-idempotency-key-header](https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-idempotency-key-header) ·
[draft-ietf-httpapi-ratelimit-headers-11](https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-ratelimit-headers)

Resilience —
[failsafe-go circuit breaker](https://failsafe-go.dev/circuit-breaker/) ·
[failsafe-go godoc](https://pkg.go.dev/github.com/failsafe-go/failsafe-go/circuitbreaker) ·
[sony/gobreaker walkthrough](https://oneuptime.com/blog/post/2026-01-07-go-circuit-breaker/view)

Observability —
[Prometheus: histograms & summaries](https://prometheus.io/docs/practices/histograms/) ·
[OTel HTTP histogram bucket defaults](https://opentelemetry.io/docs/zero-code/obi/configure/metrics-histograms/) ·
[RED & USE methods](https://timesofcloud.com/prometheus-grafana/red-use-methods/)

Protobuf → REST → OpenAPI —
[connectrpc/vanguard-go](https://github.com/connectrpc/vanguard-go) ·
[vanguard godoc](https://pkg.go.dev/connectrpc.com/vanguard) ·
[protoc-gen-connect-openapi](https://github.com/sudorandom/protoc-gen-connect-openapi) ·
[…its grpc-gateway annotation support](https://github.com/sudorandom/protoc-gen-connect-openapi/blob/main/grpcgateway.md) ·
[grpc-gateway transcoding](https://oneuptime.com/blog/post/2026-01-08-grpc-gateway-rest-transcoding/view)

go-micro —
[go-micro.dev/v5](https://pkg.go.dev/go-micro.dev/v5) ·
[go-micro.dev/v6](https://pkg.go.dev/go-micro.dev/v6) ·
[go-micro/plugins](https://github.com/go-micro/plugins) ·
[v5 http transport handler](https://pkg.go.dev/go-micro.dev/v5/server/handler/transport/http)

Internal conventions reused —
`bigstack-handbook/kb/lachesis/architecture/scenariotest-cli.md` (three logging tiers, `LevelTrace`,
POSIX flags, exit codes) ·
`.../architecture/package-map.md` (four archetypes, `workers()` table, consumer-defined interfaces) ·
`.../runbooks/live-tune-the-agent.md` (SIGHUP hot set vs restart-only, `/debug/config`,
`config_reloads_total`)
