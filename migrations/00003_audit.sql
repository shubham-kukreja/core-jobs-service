-- +goose Up
-- Append-only audit trail. Every mutation through the API writes one row in
-- the same transaction as the change. `before`/`after` hold the resource with
-- secrets removed by the auditor (password hashes, tokens never appear).

CREATE TABLE audit.audit_log (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  at          timestamptz NOT NULL DEFAULT now(),
  actor_id    bigint REFERENCES auth.users(id),       -- null for system actions
  actor_kind  text NOT NULL CHECK (actor_kind IN ('user','api_key','system')),
  request_id  text CHECK (length(request_id) <= 64),
  ip          inet,
  entity      text NOT NULL CHECK (length(entity) <= 64),   -- 'series', 'model_config', 'datapoint', 'session', …
  entity_id   text NOT NULL CHECK (length(entity_id) <= 128),
  action      text NOT NULL CHECK (length(action) <= 32),   -- create | update | retract | approve | publish | login | …
  before      jsonb,
  after       jsonb
);
CREATE INDEX audit_entity ON audit.audit_log (entity, entity_id, at DESC);
CREATE INDEX audit_actor ON audit.audit_log (actor_id, at DESC);

CREATE TRIGGER audit_log_append_only
  BEFORE UPDATE OR DELETE ON audit.audit_log
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
CREATE TRIGGER audit_log_no_truncate
  BEFORE TRUNCATE ON audit.audit_log
  FOR EACH STATEMENT EXECUTE FUNCTION platform.forbid_mutation();

-- +goose Down
DROP TABLE audit.audit_log;
