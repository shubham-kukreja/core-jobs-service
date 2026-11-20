-- +goose NO TRANSACTION
-- P4-T1-06. dp_pit covered active rows only, so GET .../versions (every row
-- for a series and date, retractions included) fell back to a table scan.
-- The index now covers all rows; point-in-time reads still filter on
-- status and mostly use dp_dedupe. Retracted rows are rare, so it grows
-- negligibly. Built CONCURRENTLY so a large table keeps taking writes.
--
-- Partition-ready: every datapoints index leads with series_id, and the
-- unique dp_dedupe already contains series_id and obs_date, so a later move
-- to PARTITION BY HASH (series_id) or RANGE (obs_date) keeps them as local
-- indexes. Only the identity primary key (and supersedes_id's FK to it)
-- would need series_id added; see docs/operations.md.

-- +goose Up
CREATE INDEX CONCURRENTLY dp_series_obs ON dp.datapoints (series_id, obs_date, known_at DESC);
DROP INDEX CONCURRENTLY dp.dp_pit;

-- +goose Down
CREATE INDEX CONCURRENTLY dp_pit ON dp.datapoints (series_id, obs_date, known_at DESC) WHERE status = 'active';
DROP INDEX CONCURRENTLY dp.dp_series_obs;
