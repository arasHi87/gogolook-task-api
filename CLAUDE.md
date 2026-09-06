# Task API — working notes

A task-list HTTP API in Go, submitted as a backend coding exercise. The
exercise is four REST endpoints; everything past that is deliberate extra, and
each piece exists because of a specific failure it prevents.

Start with [README.md](README.md), then [docs/design.md](docs/design.md).

## Before you commit

```bash
task verify     # tidy, vet, lint, unit, integration, e2e — the whole gate
```

`task verify` is not optional. Three separate times a change passed `go build`
and `go test` locally and broke the Docker build, because `go get` left `go.sum`
stale and a warm module cache hid it. `task ci` now runs the tidy check first
for exactly that reason.

The integration and end-to-end suites need a Docker daemon. `task test` alone is
unit-only and takes seconds.

## Conventions

**Comments say why, not what.** Every non-obvious decision carries the failure
it prevents. If a comment restates the code, delete it; if a line would make a
reader ask "why is it like that", answer it there.

**One file per concern.** `internal/config` is one file per config section;
`internal/app` is one file per subsystem. A change to how storage is opened
should be a diff in `storage.go`, not in the middle of six hundred lines.

**Tables for decisions with an order.** `New`'s build table and `workers()`'
drain table are the two that describe the whole process. An order spread across
straight-line code is an order nobody can review.

**Instrumentation never inverts a dependency.** Packages that are measured
declare a narrow `Observer` interface and the composition root passes
`*metrics.Guards` in. No package outside `internal/app` imports `internal/metrics`.

**Labels are bounded by construction.** Templated routes, numeric statuses,
three tiers. No ids, no error strings, no addresses — an unbounded label is an
outage, not a metrics problem.

## Tests

Three tiers, each because the one below cannot reach the failure:

- **unit** — logic, no Docker.
- **integration** — real Postgres, for things that are properties of the
  database: `SKIP LOCKED`, lease expiry, partial unique indexes.
- **[end-to-end](test/e2e/)** — the shipped binary as two processes, for the
  seams: flag parsing, the config merge, signals, the drain, and a job crossing
  a process boundary.

Nothing in the queue or e2e suites sleeps. Every wait is on an event, a log
record or a row, with a deadline that reports what it last saw.

End-to-end scenarios are written against [`test/harness`](test/harness). **If a
scenario starts to read as a sequence of `if got != want`, the missing verb
belongs in the harness**, not in the scenario.

## Commits

`type(scope): subject` — 50-character subject, 72-column body, and the body
explains why rather than restating the diff.

```
Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_0187Ptv6RdAa1ESb7itGw2L3
```

## Things that have bitten

- **`.gitignore` patterns without a leading slash match at any depth.** A bare
  `taskapi` line silently excluded `cmd/taskapi/`, so three commits did not
  build from a clean clone.
- **`http.Request.Clone` shares the body.** A retry sends an empty body with
  the right Content-Length. net/http rescues it *sometimes*, via `GetBody` on a
  fresh connection, which is why it passed locally and failed in CI.
- **Grafana panels that return nothing read as "the system is idle."**
  `task dashboards:check` runs every panel query against the live stack.
- **`sum()` over zero series returns nothing, not zero.** Use `or vector(0)` —
  but only on an ungrouped query, or it appends a phantom series forever.
- **The shell here is fish.** `>` does not clobber; `rm -f` first, or use
  `bash -c`.
