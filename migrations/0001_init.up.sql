-- The domain table.
--
-- Identity and timestamps have defaults, but the service assigns them anyway:
-- the in-memory backend has no DDL to read them from, and two backends that
-- disagree about who assigns an id will eventually disagree about the value.
-- The defaults are here as a backstop for anything that writes directly.
CREATE TABLE tasks (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL CHECK (length(name) BETWEEN 1 AND 255),
    status     smallint    NOT NULL DEFAULT 0 CHECK (status IN (0, 1)),
    -- Optimistic concurrency. A caller that sends back the version it read is
    -- told about a concurrent edit instead of silently overwriting it.
    version    bigint      NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Keyset pagination reads this index and nothing else. The id breaks ties on
-- created_at, which is what makes the ordering total: without it two rows
-- created in the same clock tick can straddle a page boundary and be returned
-- twice or not at all.
CREATE INDEX tasks_created_at_id_idx ON tasks (created_at DESC, id DESC);

-- The status filter is worth its own index only once the table is large and
-- the distribution is skewed; noted here rather than created, because an index
-- nobody uses still costs every write.


-- The queue, which is also the outbox.
--
-- The classic outbox pattern writes to a separate table and relays it to a
-- broker. There is no broker here — Postgres is the transport — so the outbox
-- row and the job row are one row. Either the task and its event both exist or
-- neither does.
--
-- retryable is deliberately distinct from available. A failed job that goes
-- straight back to available is instantly re-claimable, hot-loops the pool and
-- starves fresh work; the scheduler is what moves a retryable job back once
-- its backoff has elapsed.
--
--   available ──claim──► running ──ok────────► succeeded
--        ▲                  │
--        │                  ├──retryable──────► retryable ──(scheduled_at)──┐
--        │                  ├──terminal───────► cancelled                   │
--        │                  └──attempts spent─► discarded                   │
--        └──────────────── scheduler ──────────────────────────────────────-┘
CREATE TYPE job_state AS ENUM (
    'available',
    'scheduled',
    'running',
    'retryable',
    'succeeded',
    'cancelled',
    'discarded'
);

CREATE TABLE jobs (
    id               bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind             text        NOT NULL,
    payload          jsonb       NOT NULL,
    state            job_state   NOT NULL DEFAULT 'available',
    priority         smallint    NOT NULL DEFAULT 0,
    attempt          int         NOT NULL DEFAULT 0,
    max_attempts     int         NOT NULL DEFAULT 5 CHECK (max_attempts >= 1),
    scheduled_at     timestamptz NOT NULL DEFAULT now(),

    -- The lease. A claim stamps an expiry; a worker that dies leaves the row
    -- in running until the lease lapses, and the reaper reclaims it.
    locked_by        text,
    locked_at        timestamptz,
    lease_expires_at timestamptz,
    heartbeat_at     timestamptz,

    -- Every worker that has tried, and every error, not just the last one.
    -- "It failed four times" with no idea which worker or why is not an
    -- answer anyone can act on.
    attempted_by     text[]      NOT NULL DEFAULT '{}',
    errors           jsonb       NOT NULL DEFAULT '[]'::jsonb,

    -- Dedup key: one live job per logical event.
    unique_key       text,

    created_at       timestamptz NOT NULL DEFAULT now(),
    finalized_at     timestamptz
);

-- The only index the claim query may use. Partial, so it holds just the rows
-- that are actually claimable rather than the whole history.
CREATE INDEX jobs_claim_idx ON jobs (priority, scheduled_at, id)
    WHERE state = 'available';

-- The scheduler's index: rows that have come due.
CREATE INDEX jobs_schedule_idx ON jobs (scheduled_at)
    WHERE state IN ('retryable', 'scheduled');

-- The reaper's index. Running rows are in neither partial index above, which
-- is the point: the reaper's sweep must not scan the claimable set.
CREATE INDEX jobs_lease_idx ON jobs (lease_expires_at)
    WHERE state = 'running';

-- Enqueue-time dedup. The partial predicate is the whole design: a key is
-- unique only while a job holding it is still live, so the same logical event
-- can be enqueued again once the previous one has finalized.
CREATE UNIQUE INDEX jobs_unique_key_idx ON jobs (unique_key)
    WHERE unique_key IS NOT NULL
      AND state IN ('available', 'scheduled', 'running', 'retryable');

-- The purger's index.
CREATE INDEX jobs_finalized_idx ON jobs (finalized_at)
    WHERE state IN ('succeeded', 'cancelled', 'discarded');

-- A queue table is high-churn, and the global autovacuum thresholds are tuned
-- for tables that are mostly read. Scale factors of 0 with absolute thresholds
-- mean vacuum runs on a fixed number of dead tuples rather than a fraction of
-- a table that keeps changing size. fillfactor leaves room on the page so the
-- available -> running -> succeeded updates can be HOT and skip the indexes.
ALTER TABLE jobs SET (
    autovacuum_vacuum_scale_factor  = 0.0,
    autovacuum_vacuum_threshold     = 1000,
    autovacuum_analyze_scale_factor = 0.0,
    autovacuum_analyze_threshold    = 1000,
    fillfactor                      = 80
);


-- API-level idempotency.
--
-- The fingerprint is what distinguishes a retry from a mistake: the same key
-- with the same request is a replay, the same key with a different request is
-- the client reusing a key it should not have.
CREATE TABLE idempotency_keys (
    key           text        PRIMARY KEY,
    fingerprint   bytea       NOT NULL,
    state         text        NOT NULL CHECK (state IN ('in_progress', 'completed')),
    status_code   int,
    response_body jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL DEFAULT now() + interval '24 hours'
);

CREATE INDEX idempotency_keys_expiry_idx ON idempotency_keys (expires_at);
