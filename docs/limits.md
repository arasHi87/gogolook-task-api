# Limits

What this does not do, stated rather than discovered. Saying it unprompted is
the difference between having used a library and understanding the control
plane.

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

## A job's trace does not link back to the request that enqueued it

The job span is a root span. The request that enqueued the job finished long
before, and its trace is closed — so the two are separate traces rather than one.
Joining them means storing the W3C trace context on the `jobs` row at enqueue
time and adding a span link at claim time, which is a column, four lines and a
deliberate decision about whether a trace should span a queue boundary at all.
It is noted rather than built.

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
