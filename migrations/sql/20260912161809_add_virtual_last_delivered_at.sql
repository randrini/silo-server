-- +goose Up
-- Records the last time a virtual candidate actually delivered media bytes to
-- a client (see MarkVirtualCandidateRecovered). This is durable delivery
-- evidence, unlike failed_at, which records a transport/listing failure: NULL
-- means the candidate has never delivered. Known-good rows are retained across
-- provider re-lists even when the user did not play them last, and a single
-- later failure does not immediately brand them dead.
ALTER TABLE public.media_files
    ADD COLUMN IF NOT EXISTS last_delivered_at timestamptz;

-- +goose Down
-- Dropping the column discards delivery history: a candidate that once worked
-- is no longer distinguishable from one that never delivered. The column is
-- only an optimization/retention hint, so this is safe but lossy.
ALTER TABLE public.media_files
    DROP COLUMN IF EXISTS last_delivered_at;
