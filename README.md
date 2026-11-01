# core-jobs-service

Backend for CORE, the allocation-intelligence dashboard. One Go binary runs as
`api` or `worker` against one Postgres. It ingests data on schedules, stores
every value point-in-time, derives percentiles and scores, runs the
allocation model, and serves every CORE screen under `/v1`.

- Plan: [`docs/plan.md`](docs/plan.md)
- Security rules: [`docs/security.md`](docs/security.md)
- Status and decisions: [`HANDOFF.md`](HANDOFF.md)
- API: [`api/openapi.yaml`](api/openapi.yaml)

## Develop

Requires Go (the toolchain in `go.mod` downloads automatically), Docker and Node 20 (OpenAPI lint).

```sh
make up          # Postgres + S3, migrations, dev runtime user
make test        # all tests, including database tests
make lint        # golangci-lint (incl. gosec), govulncheck, seeds, OpenAPI
make run-api     # http://127.0.0.1:8088/healthz
make help        # everything else
```

## Layout

```
cmd/app/            entrypoint: api | worker | migrate | seed
internal/<module>/  contract.go is the module's public surface; other modules use only that
internal/platform/  config, database, logging, clock, blob, errors, paging, migrations
migrations/         goose SQL (embedded)
configs/schema/     JSON Schemas for seed and config files (embedded)
configs/seed/       seed data: entities, sources, series, model config, schedules
api/openapi.yaml    API contract
```
