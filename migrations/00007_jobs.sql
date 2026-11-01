-- +goose Up
-- Our job bookkeeping. The queue engine (River) keeps its own tables in the
-- `river` schema, created by `app migrate`; only internal/jobs/river touches them.

CREATE TABLE jobs.job_schedules (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid            uuid UNIQUE NOT NULL DEFAULT gen_random_uuid(),
  name            text UNIQUE NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9\-]{0,62}$'),
  kind            text NOT NULL CHECK (platform.is_key(kind)),
  args            jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(args) = 'object'),
  schedule_type   text NOT NULL CHECK (schedule_type IN ('cron','interval','calendar')),
  expr            text NOT NULL CHECK (length(expr) BETWEEN 1 AND 64),
  timezone        text NOT NULL DEFAULT 'Asia/Kolkata',
  overlap_policy  text NOT NULL DEFAULT 'skip' CHECK (overlap_policy IN ('skip','queue','cancel_previous')),
  catchup_policy  text NOT NULL DEFAULT 'latest' CHECK (catchup_policy IN ('none','latest','all')),
  jitter_secs     int NOT NULL DEFAULT 0 CHECK (jitter_secs BETWEEN 0 AND 3600),
  enabled         boolean NOT NULL DEFAULT true,
  next_run_at     timestamptz NOT NULL,
  last_run_id     bigint,
  created_by      bigint REFERENCES auth.users(id),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sched_due ON jobs.job_schedules (next_run_at) WHERE enabled;

CREATE TABLE jobs.job_runs (
  id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid             uuid UNIQUE NOT NULL DEFAULT gen_random_uuid(),
  kind             text NOT NULL CHECK (platform.is_key(kind)),
  args             jsonb NOT NULL CHECK (jsonb_typeof(args) = 'object'),
  args_version     int NOT NULL CHECK (args_version > 0),
  trigger          text NOT NULL CHECK (trigger IN ('schedule','event','user','chain','system')),
  schedule_id      bigint REFERENCES jobs.job_schedules(id),
  user_id          bigint REFERENCES auth.users(id),
  parent_id        bigint REFERENCES jobs.job_runs(id),
  root_id          bigint REFERENCES jobs.job_runs(id),
  idempotency_key  text CHECK (length(idempotency_key) BETWEEN 1 AND 128),
  queue            text NOT NULL CHECK (length(queue) <= 32),
  status           text NOT NULL CHECK (status IN ('queued','running','succeeded','failed','cancelled','skipped')),
  attempt          int NOT NULL DEFAULT 0,
  progress         smallint NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
  message          text CHECK (length(message) <= 500),
  result           jsonb,
  result_uri       text CHECK (result_uri IS NULL OR result_uri LIKE 'blob://%'),
  error            jsonb,          -- {"code","message","retryable"}; never a stack trace or secret
  engine_job_id    bigint,
  cancel_requested boolean NOT NULL DEFAULT false,
  queued_at        timestamptz NOT NULL DEFAULT now(),
  started_at       timestamptz,
  finished_at      timestamptz,
  expires_at       timestamptz,
  CHECK (trigger <> 'user' OR user_id IS NOT NULL),
  CHECK (trigger NOT IN ('schedule','event') OR user_id IS NULL),
  CHECK (trigger <> 'schedule' OR schedule_id IS NOT NULL)
);
CREATE UNIQUE INDEX job_idem ON jobs.job_runs (user_id, kind, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX job_root ON jobs.job_runs (root_id);
CREATE INDEX job_parent ON jobs.job_runs (parent_id);
CREATE INDEX job_kind_status ON jobs.job_runs (kind, status, queued_at DESC);

ALTER TABLE jobs.job_schedules
  ADD CONSTRAINT job_schedules_last_run_fk FOREIGN KEY (last_run_id) REFERENCES jobs.job_runs(id);
ALTER TABLE dp.datapoints
  ADD CONSTRAINT datapoints_job_run_fk FOREIGN KEY (job_run_id) REFERENCES jobs.job_runs(id);
ALTER TABLE ingest.ingest_runs
  ADD CONSTRAINT ingest_runs_job_run_fk FOREIGN KEY (job_run_id) REFERENCES jobs.job_runs(id);

-- +goose Down
ALTER TABLE ingest.ingest_runs DROP CONSTRAINT ingest_runs_job_run_fk;
ALTER TABLE dp.datapoints DROP CONSTRAINT datapoints_job_run_fk;
ALTER TABLE jobs.job_schedules DROP CONSTRAINT job_schedules_last_run_fk;
DROP TABLE jobs.job_runs, jobs.job_schedules;
