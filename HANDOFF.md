# Handoff

Shared log between the lead and teams. Newest entry first. Each entry says
what's done, what's next, and any contract change. The plan is `docs/plan.md`
and security rules are in `docs/security.md`.

## 2026-11-02 — Phase 1, T1 Data

**Done** (no contract changes)

| Task | Output |
| --- | --- |
| P1-T1-01 | `catalog.Store` on sqlc (`internal/catalog/sqlc.yaml`, `queries/`): entities, sources, series CRUD; `ListSeries` keyset paging, trailing-`*` wildcards with LIKE metacharacters escaped; errors mapped to `apperr` (`ErrNotFound` → not_found, `ErrCycle` → conflict) |
| P1-T1-02 | `series_deps` rebuilt on save under an advisory lock; cycle detection and topological `Downstream` in memory (`graph.go`); derivation version bumps (and `Rebuild=true`) only when fn/inputs/params change |
| P1-T1-03 | `datapoints.Store.Write`: per-point validation, no-op skip (same value per source; computed rows by `input_hash`), one `unnest` insert, one outbox event, all in the caller's tx; `Retract` with retraction rows and an outbox event; max 10,000 points per call |
| P1-T1-04 | PIT reader (`At/Range/History/Snapshot/Versions/Get/Lineage/Batch`); `Outbox.Claim/MarkDispatched`; `RawPayloads.Record` |
| P1-T1-05 | `blob.S3` (minio-go): SSE-S3 option, `Verify` rejects public bucket policies, streaming size-capped `Put` with sha256, partial uploads removed, keys reject traversal, URIs for other buckets rejected |
| P1-T1-07 (repo) | Registry metadata on `Series.Registry`; `SeriesFilter.Group` |

**Behaviour other teams rely on**

- **Source priority:** entries are source keys or source kinds. A kind
  expands to all sources of that kind, ordered by key.
- **Retractions:** a retraction hides only the row it supersedes, so a read
  falls back to that source's previous value. Retracting twice is
  `conflict`. Retractions emit `datapoints.written`, so the cascade
  recomputes.
- **Who may write which layer:**
  - `computed` writes derived/window/percentile/score/trigger/veto.
  - `analyst` writes raw/judgment/trigger.
  - `file_import` and external APIs write raw only.
  - `override` may write any layer.
  - Analyst and override writes require `ActorID`.
- **Null values:** only `computed` may write null ("not computable", e.g.
  below `min_obs`).
- **Datapoints never read catalog tables:** series and sources come through
  `catalog.Reader`. The source list is cached for 1 minute.

**Not verified yet:** Docker was hung for this whole task.

- `store_db_test.go` in catalog and datapoints compiles but has not run. It
  covers PIT table tests, retraction before and after the cutoff, rollback,
  outbox claim with SKIP LOCKED, and the cycle-rejection rollback.
- `TestS3RoundTrip` has not run against SeaweedFS (needs `TEST_S3_*`).
- Coverage with Docker down: catalog 41%, datapoints 19%, blob 30%. The DB
  tests are what reach 80%.
- Run `make up && make test` before merging.

**Next:**

- When G1 is wired, connect `blob.S3.Verify` at startup.
- Create the dev bucket for SeaweedFS (`make up` doesn't do that yet).

## 2026-11-01 — T7 Verify (lead, P1-T7-01/02)

**Done**

- `testdata/sheet_v2/`: the golden fixture format (see its `README.md`) and
  three scenarios built from the plan's worked numbers:
  - L1 tilts and final weights.
  - Sector sleeve carve-out.
  - Veto capping.
- `e2e/golden`: `Load` and `Compare` (0.05pp tolerance), plus an independent
  formula oracle. It proves each fixture is internally consistent, so T5 can
  trust the targets.

**Found**

- The plan's lineage example prints `tilt.equity.small = -0.101` (composite
  33.3, cap 0.30). Its own formula gives -0.1002, which is 0.08pp off and so
  outside the gate tolerance. The fixture uses the formula value; to confirm
  against the workbook.

**Next:** replace `source: plan_worked_example` with workbook fixtures once
the file is available (all 85 scores, every composite, tilt, final and
rollup weight). That task is still blocked on the workbook.

## 2026-11-01 — Phase 0 (lead)

**Done**

| Task | Output |
| --- | --- |
| P0-01 | `cmd/app` modes (`api`, `worker`, `migrate`, `seed`), platform packages (config, database, logging, clock, blob, apperr, paging, migrate, testdb), Makefile, docker-compose (Postgres 16, S3), CI, golangci-lint |
| P0-02 | `migrations/00001…00011`; River tables in schema `river`; up/down/up, grants and append-only tested in `internal/platform/migrate` |
| P0-03 | `contract.go` for catalog, datapoints, derive, ingest, jobs, model, auth, audit, market, analytics, quality, notes; `jobs` registry and error helpers implemented; mocks via `make mocks` |
| P0-04 | `api/openapi.yaml`: OpenAPI 3.1 with 98 operations; `redocly lint` is clean; every `/v1` operation declares `x-required-role` |
| P0-05 | `configs/schema/*.json`, example seeds in `configs/seed/`, `app seed validate` with cross-file checks |
| P0-06 | This file, `CODEOWNERS`, `docs/security.md` |

**Gate G0:** contracts are frozen. Teams may start Phase 1.

**Changes from the plan** (the reason follows each change)

- **Auth schema:** login is modelled as identities (`auth.identities` +
  `auth.password_credentials`), which makes it OAuth-ready without schema
  churn. Sessions store a token hash and a CSRF hash. Lockout fields are on
  `auth.users`. API keys have a prefix, expiry and last-used time.
- **Audit:** the audit log moved to its own `audit` schema, for separate
  grants. It is append-only, enforced by triggers.
- **New `ingest` schema:** it holds `uploads` and `import_mappings`, which the
  plan's API needed but its schema lacked, plus `ingest_runs` (moved from
  `dp`).
- **New table `app.idempotency_keys`:** it backs `Idempotency-Key` on all
  POSTs that create work, not only `POST /v1/jobs`.
- **Series layer `veto`:** added, because `veto.evaluate` writes `veto.*`
  series.
- **Constraints:** CHECKs require a derivation on derived/window/percentile/
  score series and forbid one on raw/judgment/veto.
- **Risk profiles:** they are versioned (`UNIQUE (key, version)`), and model
  runs record `risk_profile_id`.
- **`jobs.RunCtx` has no `datapoints.Writer`:** that keeps `jobs` depending on
  platform only. Handlers get writers through their constructor and pass
  `rc.RunID` in `WriteMeta`.
- **No `init()` registration or global `jobs.Default`:** kinds register
  explicitly at wiring time with injected dependencies, so tests construct
  isolated registries.
- **`model.Engine.Evaluate` is pure:** draft edits (`set_rubric`, …) are
  converted to inputs by `model.Drafts` before evaluation.
- **S3 for local dev:** it is SeaweedFS, because MinIO no longer publishes
  pullable images. Production uses any S3-compatible store.
- **Go toolchain:** pinned to `go1.26.10`. go1.26.0 had 25 reachable stdlib
  vulnerabilities.
- **CORE keys everywhere in seeds and config:** `l1.equity`, `equity.large`,
  `<node>::<signal>`.

**API contract decisions (P0-04)**

- **Run node keys:** they are `<step>.<node>` with CORE keys, e.g.
  `tilt.equity.small` and `veto.equity.intl`.
- **CSRF:** login sets `core_session` (HttpOnly) and `core_csrf` (readable by
  the frontend). Unsafe cookie requests must send `X-CSRF-Token` equal to the
  session's token, plus an allowed `Origin`.
- **Login 429:** it applies per email and per IP whether or not the account
  exists, so lockout never reveals which emails are registered.
- **Error statuses:**
  - 400: `validation_failed`, `invalid_params`, `not_user_triggerable`.
  - 409: `conflict`, `queue_full`, `not_available`.
  - 422: `idempotency_mismatch`.
  - `POST /v1/datapoints` returns 201 with a `rejected` list, and 400 only
    when every point is rejected.
- **Mixed-role operations:** these carry an `x-required-role-note`:
  `POST /v1/versions` (review needs analyst, publish needs admin), job cancel
  (owner or admin) and note edit/delete (author or admin).
- **Allocation weights:** they are decimal strings, so sums are exact.
- **Endpoints added beyond the plan:**
  - `GET /v1/import-mappings`
  - `GET /v1/versions/{id}`
  - `GET /v1/backtests/{id}/series`
  - `GET /v1/risk-profiles/{key}`
- **Shapes the plan left undefined:** these were designed in the spec and
  need confirmation against CORE: signals, attribution, notes, search,
  cadence, rubric answers, run compare and backtest series.

**Rules for every team**

- New tables need explicit grants to `app_runtime` in the same migration.
  There are no default privileges, on purpose.
- Repository tests use `testdb.New(t).Runtime`, which has production
  privileges, never `.Owner`.
- Never put secrets or personal data in job args, results, errors, logs or
  `catalog.sources.config`.
- Map domain errors to `apperr` codes at module boundaries.
  `apperr.Error.Message` is shown to clients.

**Open questions** (owner's answer needed before the phase noted)

1. P3: the debt and metals scorecard signals and weights. They are "…" in the
   plan, so they are not in `model_maaf_v1.yaml` yet. Source: the workbook or
   the CORE Methodology screen.
2. P3: the Multi-Asset Allocation Framework workbook itself, for the golden
   fixtures (P1-T7-01). Until then, golden tests use the plan's worked numbers.
3. From the plan, unchanged:
   - Revert mode (`all` vs `manual_and_params`).
   - Legacy backend access.
   - NAV history source and depth.
   - The extreme-score rule.
   - Percentile windows per group.
   - The revision-breadth source.
   - Publication lags per source.
4. P4: deployment target (cloud, region), and which secret manager provides
   `*_FILE` mounts.

**Next:** Phase 1. T1 (P1-T1-01…05), T2 (P1-T2-01…05), T6 (P1-T6-01, -02,
-06) and T7 (P1-T7-01, -02) start in parallel.
