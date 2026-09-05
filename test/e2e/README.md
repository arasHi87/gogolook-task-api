# End-to-end suite

The shipped binary, a real Postgres, two processes, real HTTP.

```
task e2e                        # the whole suite
task e2e -- -run TestCrashed    # one case
task e2e -- -v                  # with the processes' logs
```

Needs a Docker daemon. `go test -short` skips it, so `task test` is unaffected.

## What it starts

```
                      test binary
                           │
   ┌───────────────────────┼────────────────────────┐
   │                       │                        │
   │  webhook sink     HTTP client            pgx pool
   │  (httptest)      (the contract)        (assertions)
   └───────┬───────────────┬────────────────────────┬───┘
           │               │                        │
           │ POST /hook    │ POST /tasks            │ SELECT
           │               ▼                        │
           │        ┌─────────────┐                 │
           │        │ taskapi     │                 │
           │        │   serve     │──── INSERT ─────┤
           │        └─────────────┘   task + job    │
           │                          (one tx)      ▼
           │        ┌─────────────┐            ┌──────────┐
           └────────│ taskapi     │─── claim ──│ Postgres │
                    │   worker    │            │ (private │
                    └─────────────┘            │  per test│
                                               └──────────┘
```

Both processes are the real binary, started the way the container starts it,
configured entirely through `TASKAPI_*`. Neither knows the other exists.

Three decisions do most of the work:

- **Two processes, not one.** Two halves sharing a heap can pass a test for
  reasons that have nothing to do with the queue. Here a job genuinely crosses
  a process boundary, and `kill(2)` is available — which is the only honest way
  to test a lease.
- **Every listener binds port 0.** The process logs the port it got and the
  harness reads it back out of the log stream. Picking a "free" port in the
  test and handing it over leaves a window for something else to take it, which
  is the kind of flake that only appears in CI.
- **The receiver is a Go object, not a service.** `sink.hold()` parks every
  delivery inside its handler, so a crash lands while a request is genuinely in
  flight rather than whenever the timing works out. Nothing in this suite
  sleeps waiting for work: it waits on a delivery, on a log record, or on a
  row, always with a named deadline that reports what it last saw.

## What each test is for

| Test | The claim |
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

**A circuit breaker that never reported recovering.** Every state change logged
the same message at WARN, and the log throttler collapses repeated WARN records
by `(level, message)` — which is right for a breaker that flaps, and silently
ate the `closed` transition. Each destination state now has its own message, so
flapping is still collapsed per state while open and closed are never confused
for each other.
