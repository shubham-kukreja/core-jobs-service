-- name: GetSource :one
SELECT id, key, kind, name, active FROM catalog.sources WHERE key = $1;

-- name: ListSources :many
SELECT id, key, kind, name, active FROM catalog.sources ORDER BY key;

-- name: UpsertSource :one
INSERT INTO catalog.sources (key, kind, name, config, active)
VALUES (@key, @kind, @name, @config, @active)
ON CONFLICT (key) DO UPDATE
  SET kind = EXCLUDED.kind, name = EXCLUDED.name, config = EXCLUDED.config, active = EXCLUDED.active
RETURNING id;
