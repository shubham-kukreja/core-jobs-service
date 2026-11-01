-- +goose Up
-- Least-privilege grants for app_runtime. The runtime role can read
-- everything it needs, write only what the application writes, and can never
-- UPDATE/DELETE append-only tables or run DDL.

GRANT USAGE ON SCHEMA platform, auth, audit, catalog, dp, ingest, jobs, model, market, analytics, app TO app_runtime;
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA platform TO app_runtime;

-- Default: read + write for mutable tables.
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA
  auth, catalog, ingest, jobs, market, analytics, app TO app_runtime;
GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA model TO app_runtime;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA
  auth, audit, catalog, dp, ingest, jobs, model, market, analytics, app TO app_runtime;

-- Append-only facts: insert + read only.
GRANT SELECT, INSERT ON audit.audit_log TO app_runtime;
GRANT SELECT, INSERT ON dp.datapoints, dp.raw_payloads TO app_runtime;
REVOKE UPDATE ON model.run_values, model.allocations FROM app_runtime;

-- Outbox rows are marked dispatched, never deleted by the app (pruned by maintenance).
GRANT SELECT, INSERT, UPDATE (dispatched_at) ON dp.outbox TO app_runtime;

-- Versions, configs and overrides are soft-deleted; no hard deletes in model.
-- Catalog rows are deactivated, not deleted, except dependency edges.
REVOKE DELETE ON catalog.entities, catalog.sources, catalog.series FROM app_runtime;
-- Market data and backtests are append/update only.
REVOKE DELETE ON market.funds, market.fund_navs, market.index_levels, analytics.backtests FROM app_runtime;
-- Identity rows are only deleted through user deletion (cascade), never directly.
REVOKE DELETE ON auth.users FROM app_runtime;

-- +goose Down
REVOKE ALL ON ALL TABLES IN SCHEMA platform, auth, audit, catalog, dp, ingest, jobs, model, market, analytics, app FROM app_runtime;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA auth, audit, catalog, dp, ingest, jobs, model, market, analytics, app FROM app_runtime;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA platform FROM app_runtime;
REVOKE USAGE ON SCHEMA platform, auth, audit, catalog, dp, ingest, jobs, model, market, analytics, app FROM app_runtime;
