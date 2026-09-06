# Task API

A task-list HTTP API in Go, and — beyond the exercise — the queue, the guards
and the observability that would let someone else operate it.

```bash
go run ./cmd/taskapi all      # no Docker, no database, no configuration
curl localhost:8080/tasks
```

## The exercise

| Required | Where | Proved by |
|---|---|---|
| `GET`, `POST`, `PUT`, `DELETE` on `/tasks` | [`proto/task/v1/task.proto`](proto/task/v1/task.proto) | [`internal/runtime/contract_test.go`](internal/runtime/contract_test.go) |
| `name` (string), `status` (int, `[0,1]`) | same | `status: 2` is rejected, same test |
| Go 1.18+ | `go.mod` says **1.26** — [why](#a-note-on-the-go-version) | |
| Unit tests | 343 tests, 27 packages | `task test` |
| Dockerfile | [`Dockerfile`](Dockerfile) — distroless, non-root | `task docker:build` |
| In-memory storage | the default; Postgres is opt-in | `go run ./cmd/taskapi all` |

`internal/runtime/contract_test.go` is the file to open first. It runs every
case twice — once against `/api/v1/tasks` and once against the unversioned
`/tasks` the exercise specifies — and asserts the status, the JSON shape and the
error behaviour of all four endpoints.

## Build and run

```bash
task run                 # the API on the in-memory store
task build               # bin/taskapi
task docker:build        # the container image
task up                  # the full stack, below
task                     # every other task
```

Without `task`, everything is a plain Go command: `go run ./cmd/taskapi all`,
`go build ./cmd/taskapi`, `docker build -t taskapi .`.

The binary is one command with three modes — `serve`, `worker`, `all` — plus
`migrate` and `healthcheck`. `--help` on any of them.

## The API

```bash
$ curl -sS -X POST localhost:8080/tasks \
    -H 'Content-Type: application/json' -d '{"name":"buy milk","status":0}'
{"id":"074eed42-…","name":"buy milk","status":0,"createdAt":"…","updatedAt":"…","version":"1"}

$ curl -sS 'localhost:8080/tasks?page_size=2'
{"result":[…],"nextPageToken":"…"}

$ curl -sS -X PUT    localhost:8080/tasks/074eed42-… -d '{"name":"buy milk","status":1}'
$ curl -sS -X DELETE localhost:8080/tasks/074eed42-…
```

`status` is validated, not merely documented: `{"status":2}` is a 400. Deleting
an id that is already gone is a 404, not a silent success.

Interactive docs at `/docs`, and the OpenAPI document at `/openapi.yaml` — both
generated from the same proto the server is built from.

## Architecture

```
                    :8080  public                    :9090  admin
                      │                                │
          ┌───────────┴────────────┐        /metrics  /healthz  /readyz
          │  taskapi serve         │        /debug/config  /debug/pprof
          │  ┌──────────────────┐  │
          │  │ middleware chain │  │   metrics → tracing → auth → rate limit
          │  └────────┬─────────┘  │   → deprecation → body cap → idempotency
          │           ▼            │
          │   vanguard (REST→RPC)  │   routes and OpenAPI from one proto
          │           ▼            │
          │   task.Service ──► task.Repository
          └───────────────────────┬┘        memory │ postgres
                                  │
                       BEGIN ─────┴───── COMMIT      one transaction:
                    INSERT tasks   INSERT jobs        the task and its event
                                  │
                            ┌─────▼──────┐
                            │  Postgres  │  the outbox IS the queue table
                            └─────┬──────┘
                                  │ claim: FOR UPDATE SKIP LOCKED
          ┌───────────────────────┴┐
          │  taskapi worker        │   lease + heartbeat + reaper
          │  timeout → retry →     │   an open circuit snoozes the job
          │  circuit breaker ──────┼──────►  webhook receiver
          └────────────────────────┘
```

Two processes, one image, different subcommands. Neither knows the other
exists: they share a database and nothing else, which is what makes the queue
genuinely decoupled — kill the worker and the API keeps serving.

The full stack is `task up`: Postgres, both processes, a webhook receiver that
can be told to misbehave, Prometheus, Grafana and Tempo.

```
API        http://localhost:8080/tasks       Prometheus http://localhost:9092
docs       http://localhost:8080/docs        Grafana    http://localhost:3000
admin      http://localhost:9090/metrics     Tempo      http://localhost:3200
```

Four demos run against it, each showing one mechanism working rather than
claimed:

```bash
task demo:crash         # SIGKILL a worker mid-job; watch the reaper recover it
task demo:idempotency   # send the same write twice; watch it happen once
task demo:ratelimit     # two quota tiers, and what a refused caller is told
task demo:breaker       # break the dependency; watch the circuit open and close
```

## Tests

```bash
task test              # unit only, no Docker, a few seconds
task test:integration  # adds a real Postgres via testcontainers
task e2e               # builds the binary and runs it as two processes
task verify            # all of the above, plus tidy, vet and lint
```

343 tests. **83.5%** statement coverage with the integration suite, 63% from
unit tests alone; the gap is the code that only runs against a real database,
which is the code most worth testing against one.

Three tiers, each because the one below it cannot reach the failure: unit for
logic, integration against real Postgres for the things that are properties of
the database (`SKIP LOCKED`, lease expiry, partial unique indexes), and
[end-to-end](test/e2e/) for the seams — flag parsing, the config merge, signal
handling, the drain, and a job crossing a process boundary.

## Documentation

| | |
|---|---|
| [docs/design.md](docs/design.md) | what is here beyond the exercise, and the failure each piece prevents |
| [docs/limits.md](docs/limits.md) | what this deliberately does not do |
| [docs/layout.md](docs/layout.md) | the repository, directory by directory |
| [test/e2e/README.md](test/e2e/README.md) | the end-to-end scenarios and what each one claims |
| [internal/queue/README.md](internal/queue/README.md) | the queue, as a census against [river](https://github.com/riverqueue/river) |
| [docs/plan.md](docs/plan.md) | the architecture plan this was built from, decisions and all |

## A note on the Go version

The exercise says Go 1.18+; `go.mod` requires **1.26**, for range-over-int and
the `slog`/`testing` APIs used throughout. Any Go newer than 1.26 builds it, and
the Docker path needs no local Go at all:

```bash
docker build -t taskapi . && docker run --rm -p 8080:8080 taskapi
```

If a specific older toolchain is a hard requirement, say so — the changes are
mechanical.
