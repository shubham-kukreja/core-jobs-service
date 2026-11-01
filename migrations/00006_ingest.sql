-- +goose Up
-- Uploads, import mappings and ingest runs (owned by the ingest module).
-- Uploaded files live in the blob store; this table holds metadata only.

CREATE TABLE ingest.uploads (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid          uuid UNIQUE NOT NULL DEFAULT gen_random_uuid(),
  filename      text NOT NULL CHECK (length(filename) BETWEEN 1 AND 255),   -- sanitised display name only
  content_type  text NOT NULL CHECK (content_type IN (
                  'text/csv',
                  'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet')),
  bytes         bigint NOT NULL CHECK (bytes > 0),
  sha256        bytea NOT NULL CHECK (length(sha256) = 32),
  blob_uri      text NOT NULL CHECK (blob_uri LIKE 'blob://%'),
  uploaded_by   bigint NOT NULL REFERENCES auth.users(id),
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz      -- pruned after import retention window
);

CREATE TABLE ingest.import_mappings (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid        uuid UNIQUE NOT NULL DEFAULT gen_random_uuid(),
  key         text UNIQUE NOT NULL CHECK (platform.is_key(key)),
  name        text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
  version     int NOT NULL DEFAULT 1 CHECK (version > 0),
  mapping     jsonb NOT NULL CHECK (jsonb_typeof(mapping) = 'object'),   -- validated against configs/schema/import_mapping.json
  created_by  bigint NOT NULL REFERENCES auth.users(id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE ingest.ingest_runs (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  connector     text NOT NULL CHECK (platform.is_key(connector)),
  job_run_id    bigint,
  upload_id     bigint REFERENCES ingest.uploads(id),
  from_date     date,
  to_date       date,
  status        text NOT NULL CHECK (status IN ('running','succeeded','partial','failed')),
  rows_written  int NOT NULL DEFAULT 0,
  rows_skipped  int NOT NULL DEFAULT 0,
  error         text CHECK (length(error) <= 4000),
  started_at    timestamptz NOT NULL DEFAULT now(),
  finished_at   timestamptz,
  CHECK (from_date IS NULL OR to_date IS NULL OR from_date <= to_date)
);

ALTER TABLE dp.raw_payloads
  ADD CONSTRAINT raw_payloads_ingest_run_fk FOREIGN KEY (ingest_run_id) REFERENCES ingest.ingest_runs(id);

-- +goose Down
ALTER TABLE dp.raw_payloads DROP CONSTRAINT raw_payloads_ingest_run_fk;
DROP TABLE ingest.ingest_runs, ingest.import_mappings, ingest.uploads;
