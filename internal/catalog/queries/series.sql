-- name: GetSeries :one
SELECT sqlc.embed(s), e.key AS entity_key
FROM catalog.series s
LEFT JOIN catalog.entities e ON e.id = s.entity_id
WHERE s.key = $1;

-- name: GetSeriesForUpdate :one
SELECT id, derivation, derivation_ver FROM catalog.series WHERE key = $1 FOR UPDATE;

-- name: GetSeriesByIDs :many
SELECT sqlc.embed(s), e.key AS entity_key
FROM catalog.series s
LEFT JOIN catalog.entities e ON e.id = s.entity_id
WHERE s.id = ANY(@ids::bigint[]);

-- ListSeries pages by key. Patterns are LIKE patterns already escaped by the
-- caller (backslash escape); keys are exact matches. Empty arrays mean "any".
-- name: ListSeries :many
SELECT sqlc.embed(s), e.key AS entity_key
FROM catalog.series s
LEFT JOIN catalog.entities e ON e.id = s.entity_id
WHERE (sqlc.narg(layer)::text IS NULL OR s.layer = sqlc.narg(layer)::text)
  AND (sqlc.narg(entity_key)::text IS NULL OR e.key = sqlc.narg(entity_key)::text)
  AND (sqlc.narg(tag)::text IS NULL OR sqlc.narg(tag)::text = ANY(s.tags))
  AND (sqlc.narg(registry_group)::text IS NULL OR s.registry_group = sqlc.narg(registry_group)::text)
  AND ((cardinality(@keys::text[]) = 0 AND cardinality(@patterns::text[]) = 0)
       OR s.key = ANY(@keys::text[])
       OR s.key LIKE ANY(@patterns::text[]))
  AND s.key > @after::text
ORDER BY s.key
LIMIT @lim::int;

-- name: SeriesIDsByKeys :many
SELECT id, key FROM catalog.series WHERE key = ANY(@keys::text[]);

-- name: AllSeriesKeys :many
SELECT id, key FROM catalog.series;

-- name: UpsertSeries :one
INSERT INTO catalog.series (
  key, name, entity_id, layer, value_type, unit, frequency, align, allowed_values,
  validation, derivation, derivation_ver, min_history, source_priority, tags, meta,
  display_name, node_key, signal_key, registry_group, update_type, owner, stale_after, flags)
VALUES (
  @key, @name, sqlc.narg(entity_id), @layer, @value_type, sqlc.narg(unit), @frequency, @align, sqlc.narg(allowed_values),
  @validation, sqlc.narg(derivation), sqlc.narg(derivation_ver), sqlc.narg(min_history), @source_priority, @tags, @meta,
  sqlc.narg(display_name), sqlc.narg(node_key), sqlc.narg(signal_key), sqlc.narg(registry_group), sqlc.narg(update_type),
  sqlc.narg(owner), sqlc.narg(stale_after), @flags)
ON CONFLICT (key) DO UPDATE SET
  name = EXCLUDED.name, entity_id = EXCLUDED.entity_id, layer = EXCLUDED.layer, value_type = EXCLUDED.value_type,
  unit = EXCLUDED.unit, frequency = EXCLUDED.frequency, align = EXCLUDED.align, allowed_values = EXCLUDED.allowed_values,
  validation = EXCLUDED.validation, derivation = EXCLUDED.derivation, derivation_ver = EXCLUDED.derivation_ver,
  min_history = EXCLUDED.min_history, source_priority = EXCLUDED.source_priority, tags = EXCLUDED.tags, meta = EXCLUDED.meta,
  display_name = EXCLUDED.display_name, node_key = EXCLUDED.node_key, signal_key = EXCLUDED.signal_key,
  registry_group = EXCLUDED.registry_group, update_type = EXCLUDED.update_type, owner = EXCLUDED.owner,
  stale_after = EXCLUDED.stale_after, flags = EXCLUDED.flags, updated_at = now()
RETURNING id;

-- name: SetSeriesActive :execrows
UPDATE catalog.series SET active = @active, updated_at = now() WHERE key = @key;

-- name: AllDeps :many
SELECT series_id, depends_on_id FROM catalog.series_deps;

-- name: DeleteDeps :exec
DELETE FROM catalog.series_deps WHERE series_id = $1;

-- name: InsertDeps :exec
INSERT INTO catalog.series_deps (series_id, depends_on_id)
SELECT @series_id::bigint, unnest(@depends_on::bigint[]);

-- Serialises dependency-graph changes so concurrent saves cannot build a
-- cycle that neither transaction sees alone.
-- name: LockDepsGraph :exec
SELECT pg_advisory_xact_lock(hashtextextended('catalog.series_deps', 0));
