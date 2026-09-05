-- The reverse loses the exact bytes, which is the point of the change: a jsonb
-- round trip cannot be undone. Rows are dropped rather than converted, because
-- a replay of a re-normalised body is exactly the promise this migration
-- exists to keep, and half-keeping it is worse than starting the TTL again.
ALTER TABLE idempotency_keys
    DROP CONSTRAINT idempotency_keys_completed_has_response;

DELETE FROM idempotency_keys;

ALTER TABLE idempotency_keys
    DROP COLUMN content_type,
    DROP COLUMN response_body,
    ADD COLUMN response_body jsonb;

ALTER TABLE idempotency_keys
    ADD CONSTRAINT idempotency_keys_completed_has_response CHECK (
        state <> 'completed'
        OR (status_code IS NOT NULL AND response_body IS NOT NULL)
    );
