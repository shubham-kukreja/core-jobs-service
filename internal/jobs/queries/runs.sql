-- name: LockKey :exec
-- Serialises concurrent enqueues that share a dedupe/idempotency/pending key.
SELECT pg_advisory_xact_lock(sqlc.arg(key)::bigint);

-- name: InsertRun :one
INSERT INTO jobs.job_runs (
  kind, args, args_version, trigger, schedule_id, user_id, parent_id, root_id,
  idempotency_key, queue, status, queued_at, expires_at
) VALUES (
  sqlc.arg(kind), sqlc.arg(args), sqlc.arg(args_version), sqlc.arg(trigger),
  sqlc.narg(schedule_id), sqlc.narg(user_id), sqlc.narg(parent_id), sqlc.narg(root_id),
  sqlc.narg(idempotency_key), sqlc.arg(queue), 'queued', sqlc.arg(queued_at), sqlc.narg(expires_at)
)
RETURNING id, uuid;

-- name: SetRunRootAndEngine :exec
UPDATE jobs.job_runs
SET root_id = COALESCE(root_id, id), engine_job_id = sqlc.narg(engine_job_id)
WHERE id = sqlc.arg(id);

-- name: FindByIdempotencyKey :one
SELECT id, uuid, status, args FROM jobs.job_runs
WHERE kind = sqlc.arg(kind)
  AND idempotency_key = sqlc.arg(idempotency_key)
  AND user_id IS NOT DISTINCT FROM sqlc.narg(user_id)
ORDER BY id DESC
LIMIT 1;

-- name: FindDuplicateContaining :one
SELECT id, uuid, status FROM jobs.job_runs
WHERE kind = sqlc.arg(kind)
  AND args @> sqlc.arg(subset)::jsonb
  AND status = ANY(sqlc.arg(statuses)::text[])
  AND queued_at >= sqlc.arg(since)
ORDER BY id DESC
LIMIT 1;

-- name: FindDuplicateEqual :one
SELECT id, uuid, status FROM jobs.job_runs
WHERE kind = sqlc.arg(kind)
  AND args = sqlc.arg(args)::jsonb
  AND status = ANY(sqlc.arg(statuses)::text[])
  AND queued_at >= sqlc.arg(since)
ORDER BY id DESC
LIMIT 1;

-- name: CountPending :one
SELECT count(*) FROM jobs.job_runs
WHERE kind = sqlc.arg(kind) AND status IN ('queued', 'running');

-- name: GetRunByID :one
SELECT * FROM jobs.job_runs WHERE id = sqlc.arg(id);

-- name: GetRunByUUID :one
SELECT * FROM jobs.job_runs WHERE uuid = sqlc.arg(uuid);

-- name: MarkRunning :one
-- Moves a runnable run to running. Returns no row when the run was cancelled
-- or already finished, so the executor skips it.
UPDATE jobs.job_runs
SET status = 'running',
    attempt = sqlc.arg(attempt),
    started_at = COALESCE(started_at, sqlc.arg(now)),
    message = NULL
WHERE id = sqlc.arg(id)
  AND status IN ('queued', 'running')
  AND NOT cancel_requested
RETURNING id;

-- name: IsCancelRequested :one
SELECT cancel_requested FROM jobs.job_runs WHERE id = sqlc.arg(id);

-- name: UpdateProgress :exec
UPDATE jobs.job_runs
SET progress = sqlc.arg(progress), message = sqlc.narg(message)
WHERE id = sqlc.arg(id) AND status = 'running';

-- name: FinishRun :exec
UPDATE jobs.job_runs
SET status = sqlc.arg(status),
    progress = CASE WHEN sqlc.arg(status) = 'succeeded' THEN 100 ELSE progress END,
    message = sqlc.narg(message),
    result = sqlc.narg(result),
    result_uri = sqlc.narg(result_uri),
    error = sqlc.narg(error),
    finished_at = sqlc.arg(now)
WHERE id = sqlc.arg(id);

-- name: RequeueRun :exec
-- A retry or snooze is pending: back to queued, keeping the last error.
UPDATE jobs.job_runs
SET status = 'queued', message = sqlc.narg(message), error = sqlc.narg(error)
WHERE id = sqlc.arg(id) AND status = 'running';

-- name: RequestCancel :one
-- Queued runs are cancelled immediately; running runs are flagged and the
-- executor's watcher cancels their context.
UPDATE jobs.job_runs
SET cancel_requested = true,
    status = CASE WHEN status = 'queued' THEN 'cancelled' ELSE status END,
    finished_at = CASE WHEN status = 'queued' THEN sqlc.arg(now) ELSE finished_at END
WHERE id = sqlc.arg(id) AND status IN ('queued', 'running')
RETURNING id, status;

-- name: ActiveRunsForSchedule :many
SELECT id FROM jobs.job_runs
WHERE schedule_id = sqlc.arg(schedule_id) AND status IN ('queued', 'running');

-- name: ListRuns :many
SELECT * FROM jobs.job_runs
WHERE (sqlc.narg(kind)::text IS NULL OR kind = sqlc.narg(kind))
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(trigger)::text IS NULL OR trigger = sqlc.narg(trigger))
  AND (sqlc.narg(root_id)::bigint IS NULL OR root_id = sqlc.narg(root_id))
  AND (sqlc.narg(since)::timestamptz IS NULL OR queued_at >= sqlc.narg(since))
  AND (sqlc.narg(before_id)::bigint IS NULL OR id < sqlc.narg(before_id))
ORDER BY id DESC
LIMIT sqlc.arg(lim);

-- name: RunTree :many
WITH RECURSIVE tree AS (
  SELECT r.*, 0 AS depth FROM jobs.job_runs r WHERE r.id = sqlc.arg(root_id)
  UNION ALL
  SELECT c.*, t.depth + 1 FROM jobs.job_runs c
  JOIN tree t ON c.parent_id = t.id
  WHERE t.depth < sqlc.arg(max_depth)::int
)
SELECT * FROM tree ORDER BY depth, id
LIMIT 1000;
