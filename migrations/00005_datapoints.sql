-- +goose Up
-- Bitemporal datapoints: obs_date is the date a value describes, known_at is
-- when it became knowable. Rows are never updated or deleted; a correction is
-- a new row, a retraction is a row with status 'retracted' that supersedes
-- another.

CREATE TABLE dp.datapoints (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  series_id       bigint NOT NULL REFERENCES catalog.series(id),
  obs_date        date NOT NULL,
  known_at        timestamptz NOT NULL,
  value_num       double precision CHECK (value_num IS NULL OR value_num NOT IN ('NaN','Infinity','-Infinity')),
  value_bool      boolean,
  value_cat       text CHECK (length(value_cat) <= 64),
  source_id       bigint NOT NULL REFERENCES catalog.sources(id),
  status          text NOT NULL DEFAULT 'active' CHECK (status IN ('active','retracted')),
  derivation_ver  int,
  input_hash      bytea CHECK (input_hash IS NULL OR length(input_hash) = 32),
  inputs          bigint[],
  batch_id        uuid NOT NULL,
  job_run_id      bigint,          -- FK added in 00007 once jobs exists
  actor_id        bigint REFERENCES auth.users(id),
  note            text CHECK (length(note) <= 2000),
  supersedes_id   bigint REFERENCES dp.datapoints(id),
  created_at      timestamptz NOT NULL DEFAULT now(),
  CHECK (num_nonnulls(value_num, value_bool, value_cat) <= 1),
  CHECK (status = 'active' OR supersedes_id IS NOT NULL)
);
CREATE INDEX dp_pit ON dp.datapoints (series_id, obs_date, known_at DESC) WHERE status = 'active';
CREATE INDEX dp_retractions ON dp.datapoints (supersedes_id) WHERE status = 'retracted';
CREATE INDEX dp_batch ON dp.datapoints (batch_id);
CREATE UNIQUE INDEX dp_dedupe ON dp.datapoints (series_id, obs_date, source_id, known_at) WHERE status = 'active';

CREATE TRIGGER datapoints_append_only
  BEFORE UPDATE OR DELETE ON dp.datapoints
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
CREATE TRIGGER datapoints_no_truncate
  BEFORE TRUNCATE ON dp.datapoints
  FOR EACH STATEMENT EXECUTE FUNCTION platform.forbid_mutation();

CREATE TABLE dp.raw_payloads (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  source_id     bigint NOT NULL REFERENCES catalog.sources(id),
  ingest_run_id bigint,
  fetched_at    timestamptz NOT NULL,
  request       jsonb NOT NULL,    -- method + URL + params; credentials stripped by the connector
  blob_uri      text NOT NULL CHECK (blob_uri LIKE 'blob://%'),
  sha256        bytea NOT NULL CHECK (length(sha256) = 32),
  bytes         bigint NOT NULL CHECK (bytes >= 0)
);

-- Transactional outbox, written in the same tx as datapoints.
CREATE TABLE dp.outbox (
  id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  topic          text NOT NULL CHECK (length(topic) <= 64),
  payload        jsonb NOT NULL,
  created_at     timestamptz NOT NULL DEFAULT now(),
  dispatched_at  timestamptz
);
CREATE INDEX outbox_pending ON dp.outbox (id) WHERE dispatched_at IS NULL;

-- +goose Down
DROP TABLE dp.outbox, dp.raw_payloads, dp.datapoints;
