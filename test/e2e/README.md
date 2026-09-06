# End-to-end scenarios

The shipped binary, a real Postgres, two processes, real HTTP.

```
task e2e                        # every scenario
task e2e -- -run TestCrashed    # one
task e2e -- -v                  # with the processes' logs
```

Needs a Docker daemon. `go test -short` skips it, so `task test` is unaffected.

## How to read this, and how to add to it

The scenarios live here; the machinery lives in [`test/harness`](../harness),
whose package documentation has the architecture diagram and the three
decisions that make these deterministic.

A scenario should read as a sequence of events:

```go
sys := harness.Start(t)

sys.Webhook.Hold()
sys.API.Create("in flight", 0)
sys.Webhook.AwaitCount(1)

sys.CrashWorker()
sys.Jobs.All().AllInState("running").AllClaimed()

sys.Webhook.Release()
sys.StartWorker()
sys.WorkerLog("expired leases reclaimed")

sys.Jobs.AwaitSettled(1).AllSucceeded().EachRetried()
```

Every verb either does something to the system or asserts something about it,
and reports its own failure — so the scenario is the story and not the
plumbing. **If a scenario starts to read as a sequence of `if got != want`, the
missing verb belongs in the harness.**

One file per theme:

| File | Theme |
|---|---|
| `contract_test.go` | the four endpoints the assignment asks for |
| `delivery_test.go` | a write becomes an event becomes a webhook |
| `idempotency_test.go` | the same request twice happens once |
| `resilience_test.go` | crashes, drains, and a dependency that goes away |
| `guards_test.go` | the rate limiter and the circuit breaker |
| `observability_test.go` | what answers on which port, and what it says |

## What each scenario claims

| Scenario | The claim |
|---|---|
| `TestContractHoldsOnPostgres` | the assignment's four endpoints, on both path prefixes, on the durable backend |
| `TestEventsCrossTheProcessBoundary` | one request in, three webhooks out, delivered by a process that never saw the request |
| `TestEveryAcceptedWriteLeavesExactlyOneJob` | a 200 means the event was written too, in the same transaction, keyed by the change |
| `TestCrashedWorkerLosesNoWork` | SIGKILL mid-flight ⇒ the lease lapses, the reaper reclaims, nothing is lost and the duplicate is visible |
| `TestGracefulDrainFinishesInFlightWork` | SIGTERM mid-flight ⇒ the work finishes, no lease expires, nothing is delivered twice |
| `TestWebhookOutageRetriesThenRecovers` | a 5xx is their problem: retry with backoff and survive it |
| `TestRejectedWebhookIsNotRetried` | a 4xx is ours: terminal on the first attempt, not five |
| `TestARetriedWriteExecutesOnce` | the same `Idempotency-Key` twice ⇒ one task, one event, one webhook, a byte-identical replay |
| `TestKeyReuseIsRejectedEndToEnd` | the same key with a different request ⇒ 422, and nothing executed |
| `TestAKeyWorksAcrossBothSurfaces` | a key used on `/api/v1/tasks` and retried on `/tasks` is a retry, not reuse |
| `TestAFailedWriteDoesNotConsumeItsKey` | a rejected write leaves no key, so the client's retry can succeed |
| `TestTheRateLimiterShedsAnonymousTraffic` | the quota is enforced, and a refused caller is told the policy and when to return |
| `TestATokenBuysTheStandardQuota` | the quota follows the token, and the two tiers do not share a bucket |
| `TestTheDefaultModeNeverRejects` | no token, a stale token and a valid one all get served |
| `TestTheBreakerOpensAndTheJobsSurviveIt` | a dead dependency opens the circuit, the jobs snooze with their budget intact, and every change is delivered when it returns |
| `TestTheAdminSurfaceIsOnTheAdminPort` | `/metrics`, `/debug/config`, `/debug/log-level` and pprof answer on 9090 and **not** on 8080 |
| `TestTheMetricsMatchWhatTheAlertsQuery` | every series the alert rules and dashboards read is actually emitted, and no task id appears in a label |
| `TestOnlyTheConsumerReportsTheBacklog` | the worker publishes the queue gauges and the api does not, so two replicas cannot double count |
| `TestTheLogLevelCanBeChangedAtRuntime` | verbosity moves without a restart, and an unknown level is refused |
| `TestTheEffectiveConfigIsServedWithSecretsMasked` | the dump reflects the environment it started with, and the DSN password is not in it |

## What it deliberately does not cover

- **The container image.** These run the binary, not the image. `task up` and
  the `image` job in CI cover the Dockerfile, the entrypoint and the healthcheck.
- **Anything the unit and integration suites already pin down.** `SKIP LOCKED`,
  backoff bounds, the config merge and the in-process contract are tested where
  they live. This suite exists for the seams between them.
- **Load.** It proves behaviour, not throughput.

## What it has already caught

**A delete that looked like a replay of the update before it.** `X-Event-Id`
was the task id and version, and a delete carries the version of the row it
removed — so a delete and the preceding update shared an id, and a receiver
following our own instruction to deduplicate on that header would have silently
dropped every deletion. No unit test could see it: it needs three real changes,
delivered to a real receiver, in order. The identity now lives on
`pgrepo.Event.ID()` and is the same string the outbox deduplicates on.

**A replayed response that was gzip.** The handler compresses according to the
request's `Accept-Encoding`, and the idempotency middleware sits outside it, so
what it captured was the encoded form — replayed later without its
`Content-Encoding` and read as binary garbage. Every unit test passed, because
a plain test handler compresses nothing. Keyed writes now negotiate the
encoding away so the stored bytes are the canonical representation, servable to
any retry.

**A response writer connect-go refused to use.** The capturing writer did not
implement `http.Flusher`, which connect type-asserts. Every write returned 500
the moment the real transcoder was behind it.

**Ten dashboard panels that would have drawn nothing.** Five queried metrics
that were defined and never written — outbound latency, retries, config
reloads, and two database counters that would have needed a pgx tracer nobody
had written. The other five queried metrics that legitimately have no series on
a healthy system, so the panel read "No data" where it should have read zero.
Prometheus also stored the native histograms and dropped the classic buckets,
which silently emptied every `_bucket` query including the latency SLO rule.
`task dashboards:check` runs every panel query against the live stack and is
what found all of it.

**A circuit breaker that never reported recovering.** Every state change logged
the same message at WARN, and the log throttler collapses repeated WARN records
by `(level, message)` — which is right for a breaker that flaps, and silently
ate the `closed` transition. Each destination state now has its own message, so
flapping is still collapsed per state while open and closed are never confused
for each other.
