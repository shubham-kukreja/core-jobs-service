-- name: GetEntity :one
SELECT e.id, e.key, e.kind, e.name, e.meta, e.active, p.key AS parent_key
FROM catalog.entities e
LEFT JOIN catalog.entities p ON p.id = e.parent_id
WHERE e.key = $1;

-- name: ListEntities :many
SELECT e.id, e.key, e.kind, e.name, e.meta, e.active, p.key AS parent_key
FROM catalog.entities e
LEFT JOIN catalog.entities p ON p.id = e.parent_id
WHERE sqlc.narg(kind)::text IS NULL OR e.kind = sqlc.narg(kind)::text
ORDER BY e.key;

-- name: EntityID :one
SELECT id FROM catalog.entities WHERE key = $1;

-- name: UpsertEntity :one
INSERT INTO catalog.entities (key, kind, name, parent_id, meta, active)
VALUES (@key, @kind, @name, sqlc.narg(parent_id), @meta, @active)
ON CONFLICT (key) DO UPDATE
  SET kind = EXCLUDED.kind, name = EXCLUDED.name, parent_id = EXCLUDED.parent_id,
      meta = EXCLUDED.meta, active = EXCLUDED.active
RETURNING id;
