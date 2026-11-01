-- +goose Up
-- Models, versioned configs, risk profiles, runs, run values, allocations,
-- plus the CORE dashboard's portfolio, shared draft and versions.

CREATE TABLE model.models (
  id    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  key   text UNIQUE NOT NULL CHECK (platform.is_key(key)),   -- 'maaf'
  name  text NOT NULL CHECK (length(name) BETWEEN 1 AND 200)
);

CREATE TABLE model.model_configs (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  model_id        bigint NOT NULL REFERENCES model.models(id),
  version         int NOT NULL CHECK (version > 0),
  status          text NOT NULL CHECK (status IN ('draft','active','retired')),
  config          jsonb NOT NULL CHECK (jsonb_typeof(config) = 'object'),
  note            text CHECK (length(note) <= 2000),
  effective_from  date,
  created_by      bigint REFERENCES auth.users(id),
  created_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (model_id, version)
);
CREATE UNIQUE INDEX one_active_config ON model.model_configs (model_id) WHERE status = 'active';

CREATE TABLE model.risk_profiles (
  id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  key              text NOT NULL CHECK (platform.is_key(key)),   -- 'balanced'
  name             text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
  version          int NOT NULL CHECK (version > 0),
  neutral_weights  jsonb NOT NULL CHECK (jsonb_typeof(neutral_weights) = 'object'),
  created_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (key, version)
);

CREATE TABLE model.model_runs (
  id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid              uuid UNIQUE NOT NULL DEFAULT gen_random_uuid(),
  model_id          bigint NOT NULL REFERENCES model.models(id),
  config_id         bigint NOT NULL REFERENCES model.model_configs(id),
  risk_profile_id   bigint NOT NULL REFERENCES model.risk_profiles(id),
  as_of             date NOT NULL,
  knowledge_cutoff  timestamptz NOT NULL,
  params            jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(params) = 'object'),
  derivation_hash   bytea NOT NULL CHECK (length(derivation_hash) = 32),
  status            text NOT NULL CHECK (status IN ('running','draft','approved','published','failed','superseded')),
  warnings          jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(warnings) = 'array'),
  job_run_id        bigint REFERENCES jobs.job_runs(id),
  created_by        bigint REFERENCES auth.users(id),
  approved_by       bigint REFERENCES auth.users(id),
  created_at        timestamptz NOT NULL DEFAULT now(),
  approved_at       timestamptz,
  CHECK ((approved_by IS NULL) = (approved_at IS NULL))
);
CREATE INDEX model_runs_lookup ON model.model_runs (model_id, status, as_of DESC);

CREATE TABLE model.run_values (
  run_id        bigint NOT NULL REFERENCES model.model_runs(id),
  node_key      text NOT NULL CHECK (length(node_key) <= 160),
  value_num     double precision CHECK (value_num IS NULL OR value_num NOT IN ('NaN','Infinity','-Infinity')),
  value_bool    boolean,
  step          text NOT NULL CHECK (length(step) <= 32),
  inputs        jsonb NOT NULL,
  is_override   boolean NOT NULL DEFAULT false,
  PRIMARY KEY (run_id, node_key),
  CHECK (num_nonnulls(value_num, value_bool) <= 1)
);
CREATE TRIGGER run_values_append_only
  BEFORE UPDATE OR DELETE ON model.run_values
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

CREATE TABLE model.node_overrides (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid        uuid UNIQUE NOT NULL DEFAULT gen_random_uuid(),
  model_id    bigint NOT NULL REFERENCES model.models(id),
  node_key    text NOT NULL CHECK (length(node_key) <= 160),
  as_of       date NOT NULL,
  value_num   double precision,
  value_bool  boolean,
  reason      text NOT NULL CHECK (length(reason) BETWEEN 1 AND 2000),
  actor_id    bigint NOT NULL REFERENCES auth.users(id),
  valid_until date,
  created_at  timestamptz NOT NULL DEFAULT now(),
  deleted_at  timestamptz,
  deleted_by  bigint REFERENCES auth.users(id),
  CHECK (num_nonnulls(value_num, value_bool) = 1),
  CHECK (valid_until IS NULL OR valid_until >= as_of)
);
CREATE INDEX node_overrides_active ON model.node_overrides (model_id, node_key, as_of) WHERE deleted_at IS NULL;

CREATE TABLE model.allocations (
  run_id            bigint NOT NULL REFERENCES model.model_runs(id),
  risk_profile_id   bigint NOT NULL REFERENCES model.risk_profiles(id),
  line_item         text NOT NULL CHECK (platform.is_key(line_item)),
  group_key         text NOT NULL CHECK (platform.is_key(group_key)),
  group_weight      numeric(9,6) NOT NULL CHECK (group_weight BETWEEN 0 AND 1),
  within_group      numeric(9,6) NOT NULL CHECK (within_group BETWEEN 0 AND 1),
  portfolio_weight  numeric(9,6) NOT NULL CHECK (portfolio_weight BETWEEN 0 AND 1),
  neutral_weight    numeric(9,6) NOT NULL CHECK (neutral_weight BETWEEN 0 AND 1),
  PRIMARY KEY (run_id, risk_profile_id, line_item)
);
CREATE TRIGGER allocations_append_only
  BEFORE UPDATE OR DELETE ON model.allocations
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

-- One portfolio ('core') now; per-profile/per-client portfolios later.
CREATE TABLE model.portfolios (
  id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  key              text UNIQUE NOT NULL CHECK (platform.is_key(key)),
  name             text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
  model_id         bigint NOT NULL REFERENCES model.models(id),
  risk_profile_id  bigint NOT NULL REFERENCES model.risk_profiles(id),
  meta             jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(meta) = 'object')
);

-- One shared draft per model, edited with optimistic concurrency on rev.
CREATE TABLE model.drafts (
  model_id         bigint PRIMARY KEY REFERENCES model.models(id),
  config           jsonb NOT NULL CHECK (jsonb_typeof(config) = 'object'),
  base_version_id  bigint,
  rev              int NOT NULL DEFAULT 1 CHECK (rev > 0),
  updated_by       bigint REFERENCES auth.users(id),
  updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE model.versions (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid          uuid UNIQUE NOT NULL DEFAULT gen_random_uuid(),
  model_id      bigint NOT NULL REFERENCES model.models(id),
  portfolio_id  bigint NOT NULL REFERENCES model.portfolios(id),
  kind          text NOT NULL CHECK (kind IN ('review','published')),
  status        text NOT NULL CHECK (status IN ('active','superseded','saved','deleted')),
  label         text CHECK (length(label) <= 200),
  reason        text CHECK (length(reason) <= 2000),
  config        jsonb NOT NULL CHECK (jsonb_typeof(config) = 'object'),
  inputs        jsonb NOT NULL CHECK (jsonb_typeof(inputs) = 'object'),
  run_id        bigint REFERENCES model.model_runs(id),
  as_of         date NOT NULL,
  created_by    bigint REFERENCES auth.users(id),
  created_at    timestamptz NOT NULL DEFAULT now(),
  published_at  timestamptz,
  deleted_at    timestamptz,
  deleted_by    bigint REFERENCES auth.users(id),
  CHECK ((kind = 'published') = (published_at IS NOT NULL)),
  CHECK ((status = 'deleted') = (deleted_at IS NOT NULL))
);
CREATE UNIQUE INDEX one_active_published ON model.versions (portfolio_id) WHERE kind = 'published' AND status = 'active';
CREATE INDEX versions_list ON model.versions (portfolio_id, created_at DESC) WHERE status <> 'deleted';

ALTER TABLE model.drafts
  ADD CONSTRAINT drafts_base_version_fk FOREIGN KEY (base_version_id) REFERENCES model.versions(id);

-- +goose Down
ALTER TABLE model.drafts DROP CONSTRAINT drafts_base_version_fk;
DROP TABLE model.versions, model.drafts, model.portfolios, model.allocations, model.node_overrides,
  model.run_values, model.model_runs, model.risk_profiles, model.model_configs, model.models;
