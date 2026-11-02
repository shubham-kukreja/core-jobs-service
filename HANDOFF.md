# Handoff

Shared log between the lead and teams. Newest entry first. Each entry says
what's done, what's next, and any contract change. The plan is `docs/plan.md`
and security rules are in `docs/security.md`.

## 2026-11-02 — T6 API: P1-T6-01, P1-T6-02, P1-T6-06

**Done**

- **audit:** `audit.NewPG(pool)` records entries in the caller's transaction.
  Redaction drops `audit:"-"` fields and sensitive keys, recursively. Query
  is newest first, with keyset paging.
- **auth:**
  - `auth.NewService(auth.NewPGStore(pool, auditor), hasher, auth.NewPolicy(), clock, cfg)`
    implements `Authenticator` and `Users`. It also has `ByEmail`, used by the
    CLI.
  - Hashing is argon2id at m=64 MiB, t=3, p=2, with a 16-byte salt. A
    semaphore bounds concurrent hashes; the size is set in `auth.NewHasher`.
  - Every login path verifies exactly one hash: unknown email, no password,
    disabled and locked users all verify against a dummy hash.
  - Lockout starts after 5 failures: 15 min, doubling, capped at 24 h.
  - Sessions and CSRF tokens are 256-bit values, stored as SHA-256. Idle
    expiry is 30 min (sliding, written at most once a minute) and absolute
    expiry is 12 h. Logout, a password change, a role change and disabling
    the user all revoke sessions.
  - API keys have the form `core_<8hex>_<43 char secret>` and are stored as
    SHA-256. They can expire, and disabling the user blocks them.
  - Every state change writes its audit row in the same transaction. Failed
    logins for unknown emails are audited without the email.
- **auth/admincli:** `user create|set-password|create-api-key`. Passwords are
  read from stdin only, without echo and entered twice on a TTY. Password
  flags and positional passwords are rejected. API key secrets go to stdout,
  notes to stderr.
- **api:** `api.New(cfg, deps)` serves `/v1` with:
  - **Middleware:** request id (inbound ids only if they match
    `^[A-Za-z0-9_-]{8,64}$`), panic recovery, client IP (X-Forwarded-For only
    from trusted proxies), access log (no query strings, headers or bodies),
    security headers, an exact-origin CORS allowlist, Go's
    `http.CrossOriginProtection`, per-IP limits, and body limits.
  - **Per request:** exactly one credential, either the cookie or a bearer
    key (both together → 401). Cookie requests with unsafe methods must send
    `X-CSRF-Token`, checked against the session's hash in constant time.
    Then a per-user rate limit and a role check per route.
  - **Routes:** login, logout, me; entities; sources (with
    `connector_available`, never config); series list/get/create/patch/deps,
    where a derivation change enqueues a rebuild in the same transaction.
  - **Datapoints routes:** write, range, value at a date, versions, lineage,
    retract, snapshot. `source=override` requires admin, checked in the
    handler.
  - **Mutations:** each runs in one transaction with its audit row. An
    `Idempotency-Key` stores the response in the same transaction and
    replays it; the same key with a different request returns 422.
  - **Cursors:** HMAC-SHA256 with a key derived from `SECRET_KEY` by HKDF,
    bound to the endpoint.
- **Login throttling:** 10 attempts per IP per minute. 5 failures per email
  hash, or per IP, within 15 min returns 429, whether or not the account
  exists. A locked account returns 401 like any other failure until the
  email's failure budget is spent, so lockout cannot be used to find
  accounts.
- **Tests:** every API test response is validated against `api/openapi.yaml`.
  A test fails if a registered route is missing from the spec.

**Verified:** unit and HTTP tests, `go vet`, golangci-lint (0 issues),
govulncheck (no reachable vulnerabilities).

**Not verified:** the DB tests (`internal/{audit,auth,api}/*_db_test.go`).
Docker is down, so they skip. Run `make up && make test`.

**Wiring for cmd/app (lead)**

```go
auditor := audit.NewPG(pool)
hasher, _ := auth.NewHasher(auth.DefaultArgon2, max(2, runtime.NumCPU()/2))
acfg := auth.DefaultConfig()
acfg.SessionIdleTTL, acfg.SessionAbsoluteTTL = cfg.SessionIdleTTL, cfg.SessionAbsoluteTTL
authSvc, _ := auth.NewService(auth.NewPGStore(pool, auditor), hasher, auth.NewPolicy(), clock.System{}, acfg)
v1, _ := api.New(api.Config{
    Production: cfg.IsProduction(), CORSOrigins: cfg.CORSOrigins,
    TrustedProxies: parsePrefixes(cfg.TrustedProxyCIDRs), MaxRequestBytes: cfg.MaxRequestBytes,
    SecretKey: []byte(cfg.SecretKey.Reveal()), CookieDomain: cfg.CookieDomain,
}, api.Deps{
    DB: pool, Auth: authSvc, Users: authSvc, Audit: auditor, Catalog: <T1 catalog>,
    Reader: <T1 reader>, Writer: <T1 writer>, Rebuilds: <adapter enqueueing derive.cascade {mode:rebuild}>,
    Idempotency: api.NewPGIdempotency(pool), Connectors: <ingest registry>, Clock: clock.System{}, Log: log,
})
mux := http.NewServeMux(); mux.Handle("/v1/", v1) // keep /healthz, /readyz from healthMux
// `app user ...` → admincli.Run(ctx, authSvc, args[1:], admincli.IO{os.Stdin, os.Stdout, os.Stderr})
```

**Open items for the lead**

- **Spec gap:** only some operations with request bodies list `413`, though
  every body is limited. Add `413` (and `415` for the content-type check)
  to all body-carrying operations.
- **`cascade_job` in `POST /v1/datapoints`:** it is always `null`. The
  outbox dispatcher enqueues the cascade asynchronously, so no job id exists
  at write time.
- **Audit endpoint:** `GET /v1/audit` is not routed. Its `actor` filter takes
  a user UUID, and `auth.Users` has no lookup by UUID. Add one, or accept a
  user id.
- **Login request ids:** login audit rows carry the IP but no request id,
  because `auth.ClientInfo` has no field for it.
- **Unknown methods:** they return 404 `not_found`, since the spec has no 405
  code.

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
