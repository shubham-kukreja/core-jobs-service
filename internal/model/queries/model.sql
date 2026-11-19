-- Models ---------------------------------------------------------------------

-- name: UpsertModel :one
INSERT INTO model.models (key, name) VALUES (@key, @name)
ON CONFLICT (key) DO UPDATE SET name = EXCLUDED.name
RETURNING id;

-- name: GetModelID :one
SELECT id FROM model.models WHERE key = $1;

-- Configs --------------------------------------------------------------------

-- name: GetConfig :one
SELECT c.id, m.key AS model_key, c.version, c.status, c.config, c.note, c.effective_from, c.created_by, c.created_at
FROM model.model_configs c JOIN model.models m ON m.id = c.model_id
WHERE m.key = @model_key AND c.version = @version;

-- name: ActiveConfig :one
SELECT c.id, m.key AS model_key, c.version, c.status, c.config, c.note, c.effective_from, c.created_by, c.created_at
FROM model.model_configs c JOIN model.models m ON m.id = c.model_id
WHERE m.key = @model_key AND c.status = 'active';

-- name: ListConfigs :many
SELECT c.id, m.key AS model_key, c.version, c.status, c.config, c.note, c.effective_from, c.created_by, c.created_at
FROM model.model_configs c JOIN model.models m ON m.id = c.model_id
WHERE m.key = @model_key
ORDER BY c.version DESC;

-- name: MaxConfigVersion :one
SELECT COALESCE(max(version), 0)::int FROM model.model_configs WHERE model_id = $1;

-- name: InsertConfig :one
INSERT INTO model.model_configs (model_id, version, status, config, note, effective_from, created_by)
VALUES (@model_id, @version, @status, @config, @note, @effective_from, @created_by)
RETURNING id, created_at;

-- name: RetireActiveConfig :exec
UPDATE model.model_configs SET status = 'retired' WHERE model_id = $1 AND status = 'active';

-- name: ActivateConfig :execrows
UPDATE model.model_configs SET status = 'active'
WHERE model_id = @model_id AND version = @version AND status IN ('draft', 'retired');

-- Risk profiles --------------------------------------------------------------

-- name: LatestProfile :one
SELECT id, key, name, version, neutral_weights FROM model.risk_profiles
WHERE key = $1 ORDER BY version DESC LIMIT 1;

-- name: ProfileByID :one
SELECT id, key, name, version, neutral_weights FROM model.risk_profiles WHERE id = $1;

-- name: ListLatestProfiles :many
SELECT DISTINCT ON (key) id, key, name, version, neutral_weights FROM model.risk_profiles
ORDER BY key, version DESC;

-- name: InsertProfile :one
INSERT INTO model.risk_profiles (key, name, version, neutral_weights)
VALUES (@key, @name, @version, @neutral_weights)
RETURNING id;

-- Portfolios -----------------------------------------------------------------

-- name: GetPortfolio :one
SELECT p.id, p.key, p.name, m.key AS model_key, p.risk_profile_id
FROM model.portfolios p JOIN model.models m ON m.id = p.model_id
WHERE p.key = $1;

-- name: UpsertPortfolio :one
INSERT INTO model.portfolios (key, name, model_id, risk_profile_id)
VALUES (@key, @name, @model_id, @risk_profile_id)
ON CONFLICT (key) DO UPDATE
  SET name = EXCLUDED.name, model_id = EXCLUDED.model_id, risk_profile_id = EXCLUDED.risk_profile_id
RETURNING id;

-- Runs -----------------------------------------------------------------------

-- name: InsertRun :one
INSERT INTO model.model_runs (model_id, config_id, risk_profile_id, as_of, knowledge_cutoff, params,
  derivation_hash, status, warnings, job_run_id, created_by)
VALUES (@model_id, @config_id, @risk_profile_id, @as_of, @knowledge_cutoff, @params,
  @derivation_hash, @status, @warnings, @job_run_id, @created_by)
RETURNING id, uuid, created_at;

-- name: InsertRunValues :copyfrom
INSERT INTO model.run_values (run_id, node_key, value_num, value_bool, step, inputs, is_override)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: InsertAllocations :copyfrom
INSERT INTO model.allocations (run_id, risk_profile_id, line_item, group_key, group_weight, within_group,
  portfolio_weight, neutral_weight)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: GetRun :one
SELECT r.id, r.uuid, m.key AS model_key, c.version AS config_version, rp.key AS risk_profile_key,
  r.risk_profile_id, r.as_of, r.knowledge_cutoff, r.params, r.derivation_hash, r.status, r.warnings,
  r.job_run_id, r.created_by, r.approved_by, r.created_at, r.approved_at
FROM model.model_runs r
JOIN model.models m ON m.id = r.model_id
JOIN model.model_configs c ON c.id = r.config_id
JOIN model.risk_profiles rp ON rp.id = r.risk_profile_id
WHERE r.uuid = $1;

-- name: ListRuns :many
SELECT r.id, r.uuid, m.key AS model_key, c.version AS config_version, rp.key AS risk_profile_key,
  r.risk_profile_id, r.as_of, r.knowledge_cutoff, r.params, r.derivation_hash, r.status, r.warnings,
  r.job_run_id, r.created_by, r.approved_by, r.created_at, r.approved_at
FROM model.model_runs r
JOIN model.models m ON m.id = r.model_id
JOIN model.model_configs c ON c.id = r.config_id
JOIN model.risk_profiles rp ON rp.id = r.risk_profile_id
WHERE (sqlc.narg(model_key)::text IS NULL OR m.key = sqlc.narg(model_key))
  AND (sqlc.narg(status)::text IS NULL OR r.status = sqlc.narg(status))
  AND (sqlc.narg(from_date)::date IS NULL OR r.as_of >= sqlc.narg(from_date))
  AND (sqlc.narg(before_id)::bigint IS NULL OR r.id < sqlc.narg(before_id))
ORDER BY r.id DESC
LIMIT @lim;

-- name: RunValues :many
SELECT node_key, value_num, value_bool, step, inputs, is_override FROM model.run_values
WHERE run_id = @run_id AND (sqlc.narg(step)::text IS NULL OR step = sqlc.narg(step))
ORDER BY node_key;

-- name: RunAllocation :many
SELECT line_item, group_key, group_weight, within_group, portfolio_weight, neutral_weight
FROM model.allocations WHERE run_id = $1
ORDER BY line_item;

-- name: LockRun :one
SELECT id, model_id, risk_profile_id, status FROM model.model_runs WHERE uuid = $1 FOR UPDATE;

-- name: ApproveRun :exec
UPDATE model.model_runs SET status = 'approved', approved_by = @actor_id, approved_at = now()
WHERE id = @id;

-- name: SupersedePublished :exec
UPDATE model.model_runs r SET status = 'superseded'
WHERE r.model_id = @model_id AND r.risk_profile_id IN (
    SELECT rp2.id FROM model.risk_profiles rp2
    WHERE rp2.key = (SELECT rp.key FROM model.risk_profiles rp WHERE rp.id = @risk_profile_id))
  AND r.status = 'published' AND r.id <> @run_id;

-- name: PublishRun :exec
UPDATE model.model_runs SET status = 'published' WHERE id = $1;

-- name: PublishedBefore :many
SELECT r.uuid, r.as_of, r.params FROM model.model_runs r JOIN model.models m ON m.id = r.model_id
WHERE m.key = @model_key AND r.status IN ('published', 'superseded') AND r.as_of < @as_of
ORDER BY r.as_of DESC, r.id DESC
LIMIT @lim;

-- Overrides ------------------------------------------------------------------

-- name: ActiveOverrides :many
SELECT o.uuid, o.node_key, o.as_of, o.value_num, o.value_bool, o.reason, o.actor_id, o.valid_until
FROM model.node_overrides o JOIN model.models m ON m.id = o.model_id
WHERE m.key = @model_key AND o.deleted_at IS NULL
  AND o.as_of <= @as_of AND (o.valid_until IS NULL OR o.valid_until >= @as_of)
ORDER BY o.node_key, o.as_of DESC, o.id DESC;

-- name: InsertOverride :one
INSERT INTO model.node_overrides (model_id, node_key, as_of, value_num, value_bool, reason, actor_id, valid_until)
VALUES (@model_id, @node_key, @as_of, @value_num, @value_bool, @reason, @actor_id, @valid_until)
RETURNING uuid;

-- name: DeleteOverride :execrows
UPDATE model.node_overrides SET deleted_at = now(), deleted_by = @actor_id
WHERE uuid = @uuid AND deleted_at IS NULL;
