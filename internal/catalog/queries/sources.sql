-- name: GetSource :one
SELECT id, key, kind, name, active FROM catalog.sources WHERE key = $1;

-- name: ListSources :many
SELECT id, key, kind, name, active FROM catalog.sources ORDER BY key;
