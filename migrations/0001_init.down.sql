-- Reverse order: the enum cannot be dropped while a column still uses it.
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS jobs;
DROP TYPE IF EXISTS job_state;
DROP TABLE IF EXISTS tasks;
