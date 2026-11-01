-- +goose Up
-- Entities, sources and series: the data dictionary. Derivations are data
-- (series.derivation), validated by the derive module before save.

CREATE TABLE catalog.entities (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  key         text UNIQUE NOT NULL CHECK (platform.is_key(key)),   -- 'equity.large', 'debt.gilt', 'sector.banking', 'l1.equity'
  kind        text NOT NULL CHECK (kind IN ('asset_class','segment','bucket','metal','sector','index','macro')),
  name        text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
  parent_id   bigint REFERENCES catalog.entities(id),
  meta        jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(meta) = 'object'),
  active      boolean NOT NULL DEFAULT true,
  CHECK (parent_id IS DISTINCT FROM id)
);

CREATE TABLE catalog.sources (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  key         text UNIQUE NOT NULL CHECK (platform.is_key(key)),   -- 'fred', 'amfi', 'analyst', 'override', 'computed'
  kind        text NOT NULL CHECK (kind IN ('external_api','file_import','analyst','override','computed')),
  name        text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
  -- Non-secret settings only (base URL, rate limit). Credentials are referenced
  -- by name, e.g. {"credential_ref": "FRED_API_KEY"}, and resolved from the
  -- environment at runtime; a CHECK keeps obvious secrets out.
  config      jsonb NOT NULL DEFAULT '{}' CHECK (
                jsonb_typeof(config) = 'object'
                AND NOT (config ?| ARRAY['api_key','apikey','password','secret','token'])),
  active      boolean NOT NULL DEFAULT true
);

CREATE TABLE catalog.series (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  key             text UNIQUE NOT NULL CHECK (platform.is_key(key)),
  name            text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
  entity_id       bigint REFERENCES catalog.entities(id),
  layer           text NOT NULL CHECK (layer IN ('raw','judgment','derived','window','percentile','score','trigger','veto')),
  value_type      text NOT NULL CHECK (value_type IN ('num','bool','cat')),
  unit            text CHECK (length(unit) <= 32),
  frequency       text NOT NULL CHECK (frequency IN ('daily','weekly','monthly','quarterly','event')),
  align           text NOT NULL DEFAULT 'last' CHECK (align IN ('last','month_end','mean')),
  allowed_values  text[] CHECK (value_type = 'cat' OR allowed_values IS NULL),
  validation      jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(validation) = 'object'),
  derivation      jsonb CHECK (derivation IS NULL OR jsonb_typeof(derivation) = 'object'),
  derivation_ver  int CHECK (derivation_ver > 0),
  min_history     int CHECK (min_history > 0),
  source_priority text[] NOT NULL DEFAULT '{override,analyst,external_api,file_import,computed}',
  tags            text[] NOT NULL DEFAULT '{}',
  meta            jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(meta) = 'object'),   -- definition_breaks, …
  -- Registry metadata for the Data Points screen.
  display_name    text CHECK (length(display_name) <= 200),
  node_key        text CHECK (node_key IS NULL OR platform.is_key(node_key)),     -- 'equity.large'
  signal_key      text CHECK (signal_key IS NULL OR platform.is_key(signal_key)), -- 'relvalue'
  registry_group  text CHECK (registry_group IN ('equity','debt','metals','governance','veto')),
  update_type     text CHECK (update_type IN ('derived','rubric','analyst','constant','governance','veto','raw')),
  owner           text CHECK (length(owner) <= 64),
  stale_after     interval,
  flags           text[] NOT NULL DEFAULT '{}',
  active          boolean NOT NULL DEFAULT true,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  -- raw/judgment are entered; derived..score are computed from a derivation.
  -- Triggers may be either (flow froth is computed, currency stress is entered);
  -- veto series are written by veto.evaluate from the model config.
  CHECK (layer NOT IN ('raw','judgment','veto') OR derivation IS NULL),
  CHECK (layer NOT IN ('derived','window','percentile','score') OR derivation IS NOT NULL),
  CHECK ((derivation IS NULL) = (derivation_ver IS NULL))
);
CREATE INDEX series_layer ON catalog.series (layer) WHERE active;
CREATE INDEX series_entity ON catalog.series (entity_id);
CREATE INDEX series_node ON catalog.series (node_key, signal_key);

-- Rebuilt from series.derivation on save; cycles are rejected before insert.
CREATE TABLE catalog.series_deps (
  series_id      bigint NOT NULL REFERENCES catalog.series(id) ON DELETE CASCADE,
  depends_on_id  bigint NOT NULL REFERENCES catalog.series(id),
  PRIMARY KEY (series_id, depends_on_id),
  CHECK (series_id <> depends_on_id)
);
CREATE INDEX series_deps_reverse ON catalog.series_deps (depends_on_id);

-- +goose Down
DROP TABLE catalog.series_deps, catalog.series, catalog.sources, catalog.entities;
