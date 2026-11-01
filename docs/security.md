# Security and privacy

CORE holds allocation models and analyst judgments for a wealth business. This
document lists the controls every module must keep. A change that weakens one
needs a recorded decision in `HANDOFF.md`.

## Data classification

| Class | Examples | Rules |
| --- | --- | --- |
| Secret | Passwords, session tokens, API keys, `SECRET_KEY`, DB/S3 credentials, connector keys | Never stored in plaintext, never logged, never in git, DB config or job args |
| Personal | User email, name, IP address, user agent | Collected only for login and security monitoring; minimum retention |
| Confidential | Model configs, scores, allocations, versions, notes, backtests | Authenticated access only; every change audited |
| Public | Health probe status | `/healthz`, `/readyz` return `ok`/`unavailable` only |

## Authentication

- **Passwords:** email + password only for now. Passwords are hashed with
  argon2id (PHC string; a DB `CHECK` rejects anything else). The policy is at
  least 12 characters, a common-password list, and no password equal to the
  email.
- **Failed logins:** an unknown email and a wrong password return the same
  `401` in the same time (a dummy hash is verified). After repeated failures
  the account is locked temporarily, with backoff. Login is also rate-limited
  per IP.
- **Sessions:** each session token is 256 random bits, and only its SHA-256 is
  stored.
  - The cookie is `core_session`: `HttpOnly; Secure; SameSite=Strict; Path=/`,
    with `__Host-` semantics when there is no `COOKIE_DOMAIN`.
  - Expiry: idle 30 min, absolute 12 h.
  - A new token is issued at login. Sessions are revoked on logout, password
    change, role change and user disable.
- **CSRF:** each session has its own CSRF token, stored hashed and given to
  the frontend in the readable `core_csrf` cookie. Every unsafe method on a
  cookie-authenticated request must echo it as `X-CSRF-Token`, and Origin is
  checked against `CORS_ORIGINS`.
- **API keys:** 256-bit keys, shown once and stored as SHA-256. They can
  expire and be revoked. Keys are for automation and must be sent as
  `Authorization: Bearer`.
- **OAuth/SSO:** added later through `auth.IdentityProvider` and
  `auth.identities`. There is no self-signup; an admin links each identity to
  an existing user.
- **User management:** users are created by the admin CLI only.

## Authorisation

- Roles are `viewer < analyst < admin`, and every route declares its minimum
  role. Today everyone is admin (product decision), but the checks run on
  every request, so restricting roles later only changes data.
- Object-level rules: jobs can be cancelled by their owner or an admin; notes
  can be edited by their author or an admin.

## Database

- **Roles:**
  - The owner role runs migrations only.
  - The api and worker connect as a LOGIN user that is a member of
    `app_runtime`. That user holds only the grants in `00011_grants.sql`: no
    DDL, no `UPDATE`/`DELETE` on datapoints, run values, allocations or audit,
    and no hard deletes of catalog or users.
  - `internal/platform/migrate` tests pin this contract.
- **Append-only:** datapoints, run values, allocations and the audit log
  reject `UPDATE`/`DELETE`/`TRUNCATE` with triggers. This holds even for the
  owner role.
- **Queries:** all of them are parameterised (sqlc/pgx). No SQL is built from
  user input; identifiers in tooling go through `pgx.Identifier.Sanitize`.
- **Connections:** they use TLS outside development (`sslmode=verify-full`), a
  statement timeout of 30 s, and UTC session time zone.
- **Constraints:** CHECKs reject malformed keys, NaN/Infinity, oversized text,
  and secrets in `catalog.sources.config`. Credentials are referenced by
  environment variable name.

## HTTP API

- **Limits:** TLS only, HSTS, and server timeouts on read-header, read, write
  and idle. Header size is capped at 16 KiB; JSON bodies at `MAX_REQUEST_BYTES`
  (1 MiB) and uploads at `MAX_UPLOAD_BYTES` (20 MiB).
- **Decoding:** JSON uses `DisallowUnknownFields`. Every input has length,
  range and pattern limits, mirrored in `api/openapi.yaml`.
- **Headers:**
  - `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'`
  - `X-Content-Type-Options: nosniff`
  - `Referrer-Policy: no-referrer`
  - `Cache-Control: no-store` on authenticated responses.
- **CORS:** an exact-origin allowlist with credentials. Wildcards are rejected
  at config load.
- **Errors:** they use the public shape with a `request_id`. Internal errors
  are never returned to clients; causes and stack traces go to logs only.
- **Rate limits:** per principal and per IP. Login has a stricter limit.
- **Client IP:** taken from `X-Forwarded-For` only when the peer is in
  `TRUSTED_PROXY_CIDRS`.
- **Cursors:** pagination cursors are HMAC-signed, so clients cannot forge
  keyset positions.
- **Metrics:** `/metrics` is on a separate internal listener
  (`METRICS_ADDR`), not the public port.

## Files and formulas

- **Uploads:**
  - The content type is sniffed and must be CSV or XLSX; the filename is
    display-only, never a path.
  - Size is capped, and XLSX parsing limits inflated size, sheets, rows,
    columns and cell size (zip-bomb defence).
  - Formulas, macros and external links are never evaluated.
  - Files are stored in the private S3 bucket with server-side encryption and
    expire after the import retention window.
- **Exports:** CSV exports escape cells starting with `= + - @ \t \r`
  (formula injection).
- **Formulas:** derivation formulas (`expr`) are compiled with an empty
  environment of input aliases only, with no function calls beyond a math
  allowlist, a length limit and a timeout.
- **Notes:** notes are stored as Markdown, and the API never renders HTML.
  Clients must render with a sanitising renderer.

## Jobs

- Job args, results and error messages must not contain secrets or personal
  data beyond user ids. `jobs.RunError.Message` is safe to show users.
- User-triggered kinds check `UserTriggerable`, role, idempotency key and
  per-kind pending caps.
- Args are decoded strictly, so unknown fields fail the job permanently.

## Logging and audit

- Logs are JSON. Attributes whose keys look sensitive (password, token,
  secret, cookie, session, authorization, api_key, credential) are redacted by
  the handler. `config.Secret` redacts itself in `fmt` and `slog`.
- Every mutation writes an audit row in the same transaction, including login,
  logout, failed login (by user id when known, never the attempted password),
  role change, key creation and revocation. Before/after snapshots have
  secrets removed.

## Retention

| Data | Retention |
| --- | --- |
| Sessions (with IP, user agent) | Deleted 30 days after expiry or revocation |
| Uploads | Deleted from S3 90 days after import |
| Idempotency records | 24 hours |
| Audit log, datapoints, run values | Kept (financial record); never edited |
| Job runs | Kept 1 year; results past `ResultTTL` are pruned |

A scheduled `maintenance.prune` job enforces this (Phase 4).

## Supply chain and operations

- **Pinned versions:** the Go toolchain is pinned in `go.mod`, which is kept
  on the latest patch release. Tools are pinned in the `Makefile`. CI runs
  `govulncheck`, `gosec` (in golangci-lint) and lint on every PR, and a build
  must pass with no known reachable vulnerabilities.
- **Secrets:** they come only from the environment or `*_FILE` mounts
  (Docker/Kubernetes secrets) provided by a secret manager. `.env` files are
  ignored by git, and `docker-compose.yml` holds throwaway development values
  only.
- **Scope of `migrate down-all`:** it is refused in staging and production.
