-- name: InsertEntry :exec
INSERT INTO audit.audit_log (actor_id, actor_kind, request_id, ip, entity, entity_id, action, before, after)
VALUES (sqlc.narg('actor_id'), @actor_kind, sqlc.narg('request_id'), sqlc.narg('ip'), @entity, @entity_id, @action,
        sqlc.narg('before'), sqlc.narg('after'));

-- name: QueryEntries :many
-- Newest first; keyset on id (identity ids follow insert order).
SELECT id, at, actor_id, actor_kind, request_id, entity, entity_id, action, before, after
FROM audit.audit_log
WHERE (sqlc.narg('entity')::text IS NULL OR entity = sqlc.narg('entity'))
  AND (sqlc.narg('entity_id')::text IS NULL OR entity_id = sqlc.narg('entity_id'))
  AND (sqlc.narg('actor_id')::bigint IS NULL OR actor_id = sqlc.narg('actor_id'))
  AND (sqlc.narg('since')::timestamptz IS NULL OR at >= sqlc.narg('since'))
  AND (sqlc.narg('before_id')::bigint IS NULL OR id < sqlc.narg('before_id'))
ORDER BY id DESC
LIMIT @lim;
