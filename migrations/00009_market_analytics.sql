-- +goose Up
-- Market data for backtests (high volume, not bitemporal) and backtest runs.

CREATE TABLE market.funds (
  scheme_code  text PRIMARY KEY CHECK (scheme_code ~ '^[0-9A-Za-z]{1,20}$'),
  name         text NOT NULL CHECK (length(name) BETWEEN 1 AND 300),
  amc          text CHECK (length(amc) <= 200),
  category     text CHECK (length(category) <= 200),
  plan         text CHECK (length(plan) <= 64),
  active       boolean NOT NULL DEFAULT true
);
CREATE INDEX funds_name ON market.funds (lower(name) text_pattern_ops);

CREATE TABLE market.fund_navs (
  scheme_code  text NOT NULL REFERENCES market.funds(scheme_code),
  nav_date     date NOT NULL,
  nav          numeric(14,4) NOT NULL CHECK (nav > 0),
  PRIMARY KEY (scheme_code, nav_date)
);

CREATE TABLE market.index_levels (
  index_key   text NOT NULL CHECK (platform.is_key(index_key)),   -- nifty50, nifty100, midcap150, smallcap250, niftybank
  level_date  date NOT NULL,
  value       numeric(14,4) NOT NULL CHECK (value > 0),
  PRIMARY KEY (index_key, level_date)
);

CREATE TABLE analytics.backtests (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid         uuid UNIQUE NOT NULL DEFAULT gen_random_uuid(),
  name         text CHECK (length(name) <= 200),
  status       text NOT NULL CHECK (status IN ('queued','running','completed','failed','cancelled')),
  source       jsonb NOT NULL CHECK (jsonb_typeof(source) = 'object'),
  allocation   jsonb NOT NULL CHECK (jsonb_typeof(allocation) = 'object'),
  benchmark    text NOT NULL CHECK (platform.is_key(benchmark)),
  legs         jsonb NOT NULL CHECK (jsonb_typeof(legs) = 'array'),
  summary      jsonb,
  series_uri   text CHECK (series_uri IS NULL OR series_uri LIKE 'blob://%'),
  per_fund     jsonb,
  warnings     jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(warnings) = 'array'),
  job_run_id   bigint REFERENCES jobs.job_runs(id),
  rerun_of     bigint REFERENCES analytics.backtests(id),
  created_by   bigint NOT NULL REFERENCES auth.users(id),
  created_at   timestamptz NOT NULL DEFAULT now(),
  finished_at  timestamptz
);
CREATE INDEX backtests_active ON analytics.backtests (status) WHERE status IN ('queued','running');

-- +goose Down
DROP TABLE analytics.backtests, market.index_levels, market.fund_navs, market.funds;
