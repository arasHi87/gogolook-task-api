-- Store the replayed response as the bytes that were sent.
--
-- response_body was jsonb, which was wrong for what the column holds. jsonb is
-- a parsed value: it reorders keys, normalises whitespace and drops duplicates.
-- All of that is correct for data you query and wrong for an HTTP response you
-- promised to replay — `{"id":"a"}` comes back as `{"id": "a"}`, so a client
-- that compares its retry's body to the original, or checks a length or a
-- signature over it, sees a difference the contract said would not be there.
--
-- Nothing ever queries inside this column. It is an opaque payload, so it is
-- stored as one.
--
-- content_type joins it for the same reason: replaying a body without the
-- header that says how to read it means the replay path has to assume, and an
-- assumption that is true today is a bug the day it stops being.
ALTER TABLE idempotency_keys
    DROP CONSTRAINT idempotency_keys_completed_has_response;

ALTER TABLE idempotency_keys
    DROP COLUMN response_body,
    ADD COLUMN response_body bytea,
    ADD COLUMN content_type  text;

ALTER TABLE idempotency_keys
    ADD CONSTRAINT idempotency_keys_completed_has_response CHECK (
        state <> 'completed'
        OR (status_code IS NOT NULL AND response_body IS NOT NULL)
    );
