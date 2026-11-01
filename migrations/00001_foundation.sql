-- +goose Up
-- Schemas (one per module), the runtime role, and shared helpers.
--
-- Two roles are involved:
--   * the migration owner (whoever runs goose) owns every object and is the
--     only role allowed to run DDL;
--   * app_runtime (NOLOGIN) receives DML grants only. Deployments create a
--     LOGIN user and `GRANT app_runtime TO <user>`; the api and worker connect
--     as that user. See 00011_grants.sql.

CREATE EXTENSION IF NOT EXISTS citext;

-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_runtime') THEN
    CREATE ROLE app_runtime NOLOGIN;
  END IF;
END
$$;
-- +goose StatementEnd

CREATE SCHEMA platform;
CREATE SCHEMA auth;
CREATE SCHEMA audit;
CREATE SCHEMA catalog;
CREATE SCHEMA dp;
CREATE SCHEMA ingest;
CREATE SCHEMA jobs;
CREATE SCHEMA model;
CREATE SCHEMA market;
CREATE SCHEMA analytics;
CREATE SCHEMA app;
-- Queue engine tables; created by River's own migrator, run by `app migrate`.
CREATE SCHEMA river;

-- Nothing may be created in public (already the default since Postgres 15;
-- explicit for clusters upgraded from older versions).
REVOKE CREATE ON SCHEMA public FROM PUBLIC;

-- Append-only enforcement. Attached to tables whose rows are facts
-- (datapoints, run values, audit). Grants already deny UPDATE/DELETE to
-- app_runtime; the trigger also stops the owner role from editing history by
-- accident. TRUNCATE is covered by a statement-level trigger.
-- +goose StatementBegin
CREATE FUNCTION platform.forbid_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '% on %.% is not allowed: table is append-only',
    TG_OP, TG_TABLE_SCHEMA, TG_TABLE_NAME
    USING ERRCODE = 'insufficient_privilege';
END
$$;
-- +goose StatementEnd

-- Keys are lowercase dotted identifiers, optionally with a ::signal suffix
-- (CORE convention, e.g. 'equity.large::relvalue'). Used by CHECKs below.
-- +goose StatementBegin
CREATE FUNCTION platform.is_key(k text) RETURNS boolean
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
  SELECT k ~ '^[a-z0-9][a-z0-9_.\-]{0,126}(::[a-z0-9][a-z0-9_\-]{0,62})?$'
$$;
-- +goose StatementEnd

-- +goose Down
DROP SCHEMA river, app, analytics, market, model, jobs, ingest, dp, catalog, audit, auth CASCADE;
DROP SCHEMA platform CASCADE;
-- The REVOKE on schema public is not undone: it matches the Postgres 15+ default.
-- app_runtime is a cluster-wide role that other databases on the same server
-- may use, so it is intentionally left in place.
