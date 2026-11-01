-- name: DueSchedules :many
-- Locks due schedules so concurrent ticks never fire the same slot twice.
SELECT * FROM jobs.job_schedules
WHERE enabled AND next_run_at <= sqlc.arg(now)
ORDER BY next_run_at
LIMIT sqlc.arg(lim)
FOR UPDATE SKIP LOCKED;

-- name: AdvanceSchedule :exec
UPDATE jobs.job_schedules
SET next_run_at = sqlc.arg(next_run_at),
    last_run_id = COALESCE(sqlc.narg(last_run_id), last_run_id)
WHERE id = sqlc.arg(id);

-- name: ListSchedules :many
SELECT * FROM jobs.job_schedules ORDER BY name LIMIT 1000;

-- name: GetScheduleByUUID :one
SELECT * FROM jobs.job_schedules WHERE uuid = sqlc.arg(uuid);

-- name: GetScheduleByUUIDForUpdate :one
SELECT * FROM jobs.job_schedules WHERE uuid = sqlc.arg(uuid) FOR UPDATE;

-- name: InsertSchedule :one
INSERT INTO jobs.job_schedules (
  name, kind, args, schedule_type, expr, timezone, overlap_policy, catchup_policy,
  jitter_secs, enabled, next_run_at, created_by, created_at, updated_at
) VALUES (
  sqlc.arg(name), sqlc.arg(kind), sqlc.arg(args), sqlc.arg(schedule_type), sqlc.arg(expr),
  sqlc.arg(timezone), sqlc.arg(overlap_policy), sqlc.arg(catchup_policy), sqlc.arg(jitter_secs),
  sqlc.arg(enabled), sqlc.arg(next_run_at), sqlc.narg(created_by), sqlc.arg(now), sqlc.arg(now)
)
RETURNING *;

-- name: UpdateSchedule :one
UPDATE jobs.job_schedules
SET name = sqlc.arg(name), kind = sqlc.arg(kind), args = sqlc.arg(args),
    schedule_type = sqlc.arg(schedule_type), expr = sqlc.arg(expr), timezone = sqlc.arg(timezone),
    overlap_policy = sqlc.arg(overlap_policy), catchup_policy = sqlc.arg(catchup_policy),
    jitter_secs = sqlc.arg(jitter_secs), enabled = sqlc.arg(enabled),
    next_run_at = sqlc.arg(next_run_at), updated_at = sqlc.arg(now)
WHERE uuid = sqlc.arg(uuid)
RETURNING *;

-- name: SetScheduleEnabled :one
UPDATE jobs.job_schedules
SET enabled = sqlc.arg(enabled),
    next_run_at = COALESCE(sqlc.narg(next_run_at), next_run_at),
    updated_at = sqlc.arg(now)
WHERE uuid = sqlc.arg(uuid)
RETURNING id;
