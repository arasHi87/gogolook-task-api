# Repository layout

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
  tracing/            the OTLP provider, and the trace id everything joins on
  testenv/            testcontainers Postgres, one database per test
  admin/              health, readiness, and the debug endpoints
  config/  logging/   one file per config section; three-tier structured logs
  httpx/  apperr/     middleware primitives; the shared error vocabulary
deploy/               compose, Prometheus rules, Tempo, provisioned Grafana
test/harness/         the machinery the end-to-end scenarios are written against
test/e2e/             the scenarios themselves
test/demo/            crash, idempotency, rate limit, breaker
docs/                 this, plus design.md and limits.md
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
