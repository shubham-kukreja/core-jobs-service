# Operations

How to run, watch, back up and change the system. For the analyst's monthly
cycle, see [runbook-monthly-upload.md](runbook-monthly-upload.md).

## Environments

| | Where | `APP_ENV` | Secrets |
| --- | --- | --- | --- |
| Dev | `docker-compose.yml` + `make run-api` / `make run-worker` | `development` | Throwaway values in the Makefile |
| Local stack ("staging" for now) | `deploy/stack/compose.yml` | `development` (no TLS on one machine) | `deploy/stack/stack.env` (gitignored) |
| Hosted staging / production | Not chosen yet | `staging` / `production` | Environment variables, or `*_FILE` mounts |

`APP_ENV=staging` or `production` enforces the strict settings:
- `sslmode=verify-full` on both database URLs
- TLS and server-side encryption on the blob store
- https-only CORS origins
- secure cookies

The local stack can't satisfy these without a CA, so it runs as `development`.
Everything else matches a real deployment:
- a separate runtime database role
- a read-only, non-root container with no capabilities
- metrics on an internal listener
- tracing

## Local stack

```sh
cp deploy/stack/stack.env.example deploy/stack/stack.env   # set every value: openssl rand -base64 36
make stack-init     # build, start Postgres + S3, create the bucket, migrate, create the admin, seed
make stack-up       # start, or rebuild and restart after a code change
make stack-health   # scheduled runs per day for the last 7 days
make stack-down     # stop (data is kept in Docker volumes)
```

| Service | URL (loopback only) |
| --- | --- |
| API | http://127.0.0.1:8080/v1, plus `/healthz` |
| Grafana | http://127.0.0.1:3000, user `admin`, password from `GRAFANA_ADMIN_PASSWORD` |
| Prometheus | http://127.0.0.1:9095 |
| Jaeger | http://127.0.0.1:16686 |
| Postgres | 127.0.0.1:55432 (owner role, for maintenance) |

`stack-init` is safe to re-run:
- migrations and the seed only add what is missing
- an existing admin is kept, though its password is reset to `ADMIN_PASSWORD`

The seed creates the schedules, enabled. They fire from the worker's
one-minute scheduler tick.

**Gate G4 acceptance:** `make stack-health` shows 7 consecutive days with
`failed = 0`. The Grafana tile "Scheduled job failures (7d)" must stay at 0.

## Secrets

Secrets are read only from the environment. Each one can instead be read from
a file named by `<NAME>_FILE`, for Docker or Kubernetes secrets. They are
never logged: `config.Secret` redacts them in fmt and slog.

| Variable | Purpose | Rotation |
| --- | --- | --- |
| `DATABASE_URL` | Runtime role (DML only) | `ALTER ROLE core_app PASSWORD …`, then restart api and worker |
| `MIGRATION_DATABASE_URL` | Owner role (DDL), used by `migrate` only | Same, for `core_owner` |
| `SECRET_KEY` (≥ 32 bytes) | CSRF tokens and cursor signing | Rotating it invalidates sessions' CSRF tokens and open cursors; users sign in again |
| `BLOB_ACCESS_KEY` / `BLOB_SECRET_KEY` | Upload storage | Change in the store, then restart |

API keys are per user (`app user create-api-key`). Their raw value is shown
once and stored only as a hash.

## Observability

| Signal | Where | Notes |
| --- | --- | --- |
| Metrics | `METRICS_ADDR` (`/metrics`, `/healthz`, `/readyz`) | Internal listener, never the public port. Labels hold kinds, routes and status codes, never ids or user data |
| Traces | OTLP/HTTP to `OTEL_EXPORTER_OTLP_ENDPOINT` | Off when unset. One server span per request and one consumer span per job attempt. The standard `OTEL_*` variables apply (e.g. `OTEL_TRACES_SAMPLER`) |
| Logs | stderr, JSON | One line per request. Query strings, headers and bodies are never logged |

Key metrics:
- `core_jobs_finished_total{kind,trigger,status}`
- `core_jobs_duration_seconds{kind,status}`
- `core_jobs_depth{kind,status}` and `core_jobs_oldest_seconds` (worker only)
- `core_outbox_depth` and `core_outbox_oldest_seconds`
- `core_http_requests_total{method,route,code}`
- `core_http_request_duration_seconds`
- `core_db_pool_*`

Suggested alerts:
- any increase of `core_jobs_finished_total{trigger="schedule",status="failed"}`
- `core_jobs_oldest_seconds{status="queued"} > 1800`
- `core_outbox_oldest_seconds > 600`
- `core_jobs_up == 0`
- `/readyz` failing for more than 2 minutes

## Backups

Postgres holds everything that matters. Uploaded files are kept only until
they are imported and then expire.

```sh
docker compose -f deploy/stack/compose.yml exec -T postgres pg_dump -U core_owner -Fc core > core-$(date +%F).dump
# restore into an empty database:
docker compose -f deploy/stack/compose.yml exec -T postgres pg_restore -U core_owner -d core --clean --if-exists < core-YYYY-MM-DD.dump
```

Datapoints and audit rows are append-only, enforced by triggers even for the
owner. A restore therefore brings back exactly what was known when the dump
was taken.

## Schema changes

Migrations are embedded in the binary and applied by `app migrate up` (the
`migrate` service) with the owner role.

On a large table, build indexes with `CREATE INDEX CONCURRENTLY` in a
`-- +goose NO TRANSACTION` migration, as `00012_dp_versions_index.sql` does.
Writes then keep flowing while the index builds.

## Data volume and partitioning

Point-in-time reads were measured at 10M datapoints (`make perf`; results in
HANDOFF.md) and need no partitioning at that size. Every `dp.datapoints`
index leads with `series_id`, and the unique `dp_dedupe` already includes
`series_id` and `obs_date`. So moving to `PARTITION BY HASH (series_id)`
(or `RANGE (obs_date)`) keeps all of them as local indexes. That move needs:

1. The primary key to become `(series_id, id)`, and `supersedes_id` to
   become a `(series_id, supersedes_id)` foreign key. A retraction is always
   for the same series, so this holds.
2. Lookups by `id` alone (`GET /v1/datapoints/{id}`, lineage) to carry
   `series_id`. They already read through `Reader.Get` and the lineage walk,
   so this is a change in one package.

Revisit this at about 100M rows, or if the PIT range p95 passes 20 ms.
