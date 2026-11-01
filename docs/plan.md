# Allocation Intelligence Backend — Build Plan

Nov 1, 2026 · @Shubham Kukreja

## Overview

We will turn the Multi-Asset Allocation Framework spreadsheet into a Go modular monolith on Postgres, built in 6 phases (P0–P5) by a lead and 7 agent teams, starting with 5 job types that need no external data source. The MVP ingests data on schedules, stores every datapoint point-in-time, computes percentiles and scores automatically, runs the allocation model on demand, and lets analysts enter or override any value.

**In scope (MVP)**

- Every CORE dashboard screen served through `/v1` (see Dashboard support): shared draft, saved and published versions, live preview, explain, signal analytics, backtests, notes, search
- Six job types: `import.file`, `derive.cascade`, `veto.evaluate`, `model.run`, `quality.check`, `backtest.run`. Connectors plug in later as new kinds without core changes.
- One bitemporal `datapoints` entity for every value: file import, analyst, override, computed, external
- Derivation engine: spreads, ratios, rolling windows, percentiles, scores, veto triggers
- Model engine: one pure `Evaluate` path used by runs, versions, preview and analytics
- Migration of the current backend's data (from Phase 2, not a priority)
- Email + password auth; one portfolio now, extensible to profiles and clients

**Out of scope (later)**

- Rebuilt source connectors (FRED, AMFI, RBI/CCIL, NSE, bullion): the existing fetchers will change, so Phase 5 adds them per source
- Per-client allocations, actual holdings and drift
- Second-person approval, notifications beyond logs

**Stack**

| Concern | Choice | Why |
| --- | --- | --- |
| Language | Go 1.23+ | Team choice; single binary |
| Database | Postgres 16 | Relational + JSONB; small data (\~180 series); also hosts the queue |
| Queue | River (behind our own `jobs` interface) | Postgres-backed, transactional enqueue, periodic + unique jobs |
| SQL | pgx v5 + sqlc | Typed queries, no ORM |
| Migrations | goose | Plain SQL files |
| HTTP | chi | Small, idiomatic |
| Expressions | expr-lang/expr | Sandboxed formulas editable as data |
| Decimals | shopspring/decimal for weights; float64 for scores | Weights must sum exactly to 100% |
| Blobs | S3-compatible (MinIO locally) | Raw payloads, large results |
| Observability | slog + OpenTelemetry + Prometheus | Standard |

**Design rules**

1. Registries, never switch statements: connectors, primitives, job kinds, model steps, constraints, alert sinks.
2. Data over code: series, derivations, entities, scorecards, rules and schedules live in versioned DB config.
3. Generic jobs parameterized by data: `ingest.fetch`, `derive.cascade`, `model.run` take keys.
4. One write path for datapoints: validate → store → emit event, in one transaction.
5. Events are the seam: new features subscribe; producers never change.
6. Version everything: configs, derivations, job args, APIs (`/v1`).
7. Modules own their tables and talk through interfaces; no cross-module joins.

## Concepts

Every value in the spreadsheet maps to one of these nodes. The split rule: if a value changes when you edit Weights & Rules, it is a run value; otherwise it is a datapoint.

**Data**

| Node | What it is | In the sheet |
| --- | --- | --- |
| Entity | A thing values are about: asset class, segment, debt bucket, metal, sector, index | Column headers: Large Cap, Gilt / Long, IT Services |
| Series | A named quantity tracked over time | One column, e.g. Nifty 100 P/E |
| Datapoint | One value of a series on one date, from one source | One cell, e.g. Jan-2026 = 22.50 |
| Source | Origin of a datapoint: external\_api, file\_import, analyst, override, computed | Who typed or pulled the cell |
| known\_at | When the value became knowable | Missing in the sheet, which causes look-ahead in its percentiles |
| Source priority | Which source wins when several exist for one series and date | Retyping a cell, but the old value is kept |

**Series layers** (all stored as datapoints)

| Layer | What it is | In the sheet |
| --- | --- | --- |
| raw | Observed fact | P/E, yields, gold price, AMFI flows |
| judgment | Analyst's call, usually categorical | RBI stance, growth vs expectations |
| derived | Same-date math | Mid–Large spread, AAA spread, gold/silver ratio, real yield |
| window | Math over time | 3M avg flows % AUM, gold 12M return, TIPS 6M change |
| percentile | Rank vs own history, point-in-time | %ile columns in Data Trackers |
| score | 0–100 attractiveness | Blue cells on the scoring sheets (85 in total) |
| trigger | Veto condition, true or false | The reason behind a veto Y |

**Calculation**

| Node | What it is | In the sheet |
| --- | --- | --- |
| Derivation | Formula that builds a series from other series | Cell formula |
| Primitive | Reusable Go function a derivation calls | PERCENTRANK, AVERAGE, rubric lookup |
| series\_deps | Which series feed which | Excel precedent arrows |

**Model**

| Node | What it is | In the sheet |
| --- | --- | --- |
| Model | A strategy definition; MVP has one | The whole workbook |
| Model config | Versioned scorecards, weights, neutral weights, caps, thresholds, vetoes, constraints | Weights & Rules |
| Scorecard | Buckets × signals with weights and a tilt cap | One scoring sheet |
| Model run | One calculation for an as-of date with one config | Reading the sheet on review day |
| Run value | Every number a run produced, with its inputs | Composite, tilt, preliminary, final rows |
| Run parameter | Human decision fed into a run | Sectors held |
| Allocation | Final weights for a risk profile | Portfolio Rollup |
| Risk profile | Neutral weights per investor type | Neutral Weight columns |

**Jobs**

| Node | What it is | In the sheet |
| --- | --- | --- |
| Job kind | Code that does one type of work | You doing the monthly logging |
| Trigger | What started a job: schedule, event, user, chain, system | — |
| Schedule | DB row saying when a kind runs, with args | Monitoring & Review cadence |
| Job run | One execution: status, progress, result | "Logged Mar on 10 Apr" |
| Connector | Code that fetches one external source | You visiting NSE, AMFI, RBI |
| Event | "These series changed" signal | Excel recalculating |

## Architecture

One Go binary runs in two modes, `api` and `worker`, against one Postgres. Nine modules own their tables and expose Go interfaces; nothing joins across module tables.

&#91;embedded content: system architecture · 5 layers, one write path\]

Scheduled fetches, imports and analyst edits all land through the writer; jobs read outbox events, write computed datapoints back through the same writer, and store run values.

**Modules**

| Module | Owns tables | Exposes | Depends on |
| --- | --- | --- | --- |
| `platform` | — | DB pool, tx helper, config, logger, blob store, clock | — |
| `catalog` | entities, sources, series, series\_deps | `Catalog` (get/list series, deps graph, validation rules) | platform |
| `datapoints` | datapoints, raw\_payloads, outbox | `Writer`, `Reader` (PIT reads, history windows) | catalog, platform |
| `derive` | — (writes via datapoints) | `Primitive` registry, `Cascade` planner | catalog, datapoints |
| `ingest` | ingest\_runs | `Connector` registry | datapoints, catalog |
| `model` | models, model\_configs, model\_runs, run\_values, allocations, risk\_profiles | `Engine`, `Step` registry, `Constraint` registry | datapoints, catalog |
| `jobs` | job\_runs, job\_schedules, River tables | `Registry`, `Enqueuer`, `Spec`, `RunCtx` | platform |
| `auth` + `audit` | users, api\_keys, audit\_log | Middleware, `Auditor` | platform |
| `api` | — | HTTP handlers `/v1/*` | all of the above via interfaces |

**Repo layout**

```
cmd/app/main.go              # `app api` | `app worker` | `app migrate` | `app seed`
internal/
  platform/                  # db, tx, config, log, blob, clock
  catalog/                   # entities, series, sources, deps graph
  datapoints/                # writer, reader (PIT), outbox, raw payloads
  derive/
    primitives/              # percentile.go, rolling.go, arith.go, rubric.go …
    cascade.go               # dependency planner + executor
  ingest/
    connector.go             # interface + registry
    fred/ amfi/ rbi/ nse/ bullion/ fileimport/
  model/
    engine.go  steps/  constraints/  config.go
  jobs/
    jobs.go registry.go middleware.go scheduler.go
    river/                   # the ONLY package importing River
    kinds/                   # one file per job kind
  auth/  audit/
  api/                       # chi router, one file per resource
migrations/                  # goose SQL
queries/                     # sqlc .sql per module
configs/seed/                # entities, series, derivations, model config v1, schedules (YAML)
testdata/                    # sheet fixtures, golden outputs
```

**Request and data flow**

1. An analyst uploads the monthly tracker file (`import.file`) or enters values through `/v1/datapoints` → `datapoints.Writer` stores them and writes an outbox event in the same transaction. Connectors will use this same path in Phase 5.
2. The outbox dispatcher enqueues one `derive.cascade` per write batch (debounced, unique).
3. The cascade walks `series_deps` in topological order and writes computed datapoints (derived → window → percentile → score → trigger); trigger writes enqueue `veto.evaluate`.
4. On review dates, or when a user asks, `model.run` reads datapoints point-in-time, applies the config, and stores run values and allocations.
5. `quality.check` runs on a schedule and lists what is stale or missing, which is the analyst's to-do list until connectors exist.

## Database schema

Postgres 16, one database, one schema per module (`catalog`, `dp`, `model`, `jobs`, `auth`). Datapoints and run values are append-only. Ids are `bigint` identity except public resources, which also carry a `uuid`.

**catalog**

```sql
CREATE TABLE catalog.entities (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  key         text UNIQUE NOT NULL,          -- 'eq.midcap', 'debt.gilt', 'sector.capgoods', 'idx.nifty100'
  kind        text NOT NULL,                 -- asset_class | segment | bucket | metal | sector | index | macro
  name        text NOT NULL,
  parent_id   bigint REFERENCES catalog.entities(id),   -- eq.midcap → asset.equity
  meta        jsonb NOT NULL DEFAULT '{}',
  active      boolean NOT NULL DEFAULT true
);

CREATE TABLE catalog.sources (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  key         text UNIQUE NOT NULL,          -- 'fred', 'amfi', 'analyst', 'override', 'computed'
  kind        text NOT NULL CHECK (kind IN ('external_api','file_import','analyst','override','computed')),
  config      jsonb NOT NULL DEFAULT '{}',   -- base URL, rate limit; secrets by reference only
  active      boolean NOT NULL DEFAULT true
);

CREATE TABLE catalog.series (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  key             text UNIQUE NOT NULL,      -- 'pe.nifty100', 'pctile.180m.spread.mid_large', 'score.l1.equity.valuation'
  name            text NOT NULL,
  entity_id       bigint REFERENCES catalog.entities(id),
  layer           text NOT NULL CHECK (layer IN ('raw','judgment','derived','window','percentile','score','trigger')),
  value_type      text NOT NULL CHECK (value_type IN ('num','bool','cat')),
  unit            text,                      -- 'x', '%', 'bps', 'INR/10g', 'INR cr', 'score'
  frequency       text NOT NULL,             -- daily | weekly | monthly | quarterly | event
  align           text NOT NULL DEFAULT 'last',   -- last | month_end | mean: how to read at a coarser frequency
  allowed_values  text[],                    -- for cat, e.g. {cutting,hold,hiking}
  validation      jsonb NOT NULL DEFAULT '{}',    -- {"min":0,"max":100}
  derivation      jsonb,                     -- null for raw/judgment; see Datapoints & derivations
  derivation_ver  int,
  min_history     int,                       -- percentile guard: null result below this many obs
  source_priority text[] NOT NULL DEFAULT '{override,analyst,external_api,file_import,computed}',
  tags            text[] NOT NULL DEFAULT '{}',
  meta            jsonb NOT NULL DEFAULT '{}',
  active          boolean NOT NULL DEFAULT true,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE catalog.series_deps (            -- rebuilt from series.derivation on save
  series_id      bigint NOT NULL REFERENCES catalog.series(id),
  depends_on_id  bigint NOT NULL REFERENCES catalog.series(id),
  PRIMARY KEY (series_id, depends_on_id)
);
```

**dp (datapoints)**

```sql
CREATE TABLE dp.datapoints (
  id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  series_id          bigint NOT NULL REFERENCES catalog.series(id),
  obs_date           date NOT NULL,              -- IST calendar date the value describes
  known_at           timestamptz NOT NULL,       -- when it became knowable (computed: max of inputs)
  value_num          double precision,
  value_bool         boolean,
  value_cat          text,
  source_id          bigint NOT NULL REFERENCES catalog.sources(id),
  status             text NOT NULL DEFAULT 'active' CHECK (status IN ('active','retracted')),
  derivation_ver     int,                        -- computed rows only
  input_hash         bytea,                      -- computed rows: sha256 of input datapoint ids
  inputs             bigint[],                   -- computed rows: lineage
  batch_id           uuid NOT NULL,              -- one per write call
  job_run_id         bigint,
  actor_id           bigint,                     -- user for analyst/override rows
  note               text,
  supersedes_id      bigint REFERENCES dp.datapoints(id),
  created_at         timestamptz NOT NULL DEFAULT now(),
  CHECK (num_nonnulls(value_num, value_bool, value_cat) <= 1)
);
CREATE INDEX dp_pit ON dp.datapoints (series_id, obs_date, known_at DESC) WHERE status = 'active';
CREATE INDEX dp_batch ON dp.datapoints (batch_id);
CREATE UNIQUE INDEX dp_dedupe ON dp.datapoints (series_id, obs_date, source_id, known_at);

CREATE TABLE dp.raw_payloads (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  source_id     bigint NOT NULL REFERENCES catalog.sources(id),
  ingest_run_id bigint,
  fetched_at    timestamptz NOT NULL,
  request       jsonb NOT NULL,
  blob_uri      text NOT NULL,
  sha256        bytea NOT NULL,
  bytes         int NOT NULL
);

CREATE TABLE dp.outbox (                      -- written in the same tx as datapoints
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  topic       text NOT NULL,                  -- 'datapoints.written'
  payload     jsonb NOT NULL,                 -- {batch_id, series_ids[], min_date, max_date, origin}
  created_at  timestamptz NOT NULL DEFAULT now(),
  dispatched_at timestamptz
);
CREATE INDEX outbox_pending ON dp.outbox (id) WHERE dispatched_at IS NULL;

CREATE TABLE dp.ingest_runs (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  connector     text NOT NULL,
  job_run_id    bigint,
  from_date     date, to_date date,
  status        text NOT NULL,                -- running | succeeded | partial | failed
  rows_written  int NOT NULL DEFAULT 0,
  rows_skipped  int NOT NULL DEFAULT 0,
  error         text,
  started_at    timestamptz NOT NULL DEFAULT now(),
  finished_at   timestamptz
);
```

**model**

```sql
CREATE TABLE model.models (
  id    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  key   text UNIQUE NOT NULL,                 -- 'maaf'
  name  text NOT NULL
);

CREATE TABLE model.model_configs (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  model_id        bigint NOT NULL REFERENCES model.models(id),
  version         int NOT NULL,
  status          text NOT NULL CHECK (status IN ('draft','active','retired')),
  config          jsonb NOT NULL,             -- full schema in Model engine
  effective_from  date,
  created_by      bigint, created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (model_id, version)
);
CREATE UNIQUE INDEX one_active_config ON model.model_configs (model_id) WHERE status = 'active';

CREATE TABLE model.risk_profiles (
  id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  key              text UNIQUE NOT NULL,    -- 'balanced'
  name             text NOT NULL,
  version          int NOT NULL,
  neutral_weights  jsonb NOT NULL           -- {"asset.equity":0.55,"asset.debt":0.35,"asset.metals":0.10, "eq.large":0.45, …}
);

CREATE TABLE model.model_runs (
  id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid              uuid UNIQUE NOT NULL DEFAULT gen_random_uuid(),
  model_id          bigint NOT NULL REFERENCES model.models(id),
  config_id         bigint NOT NULL REFERENCES model.model_configs(id),
  as_of             date NOT NULL,
  knowledge_cutoff  timestamptz NOT NULL,
  params            jsonb NOT NULL DEFAULT '{}',   -- {"sectors_held":["sector.capgoods"]}
  derivation_hash   bytea NOT NULL,               -- hash of derivation versions used
  status            text NOT NULL CHECK (status IN ('running','draft','approved','published','failed','superseded')),
  warnings          jsonb NOT NULL DEFAULT '[]',
  job_run_id        bigint,
  created_by        bigint, approved_by bigint,
  created_at        timestamptz NOT NULL DEFAULT now(),
  approved_at       timestamptz
);

CREATE TABLE model.run_values (
  run_id        bigint NOT NULL REFERENCES model.model_runs(id),
  node_key      text NOT NULL,              -- 'l1.composite.asset.equity', 'eq.tilt.eq.small'
  value_num     double precision,
  value_bool    boolean,
  step          text NOT NULL,              -- composite | veto | tilt | constraints | normalize | rollup
  inputs        jsonb NOT NULL,             -- {"datapoints":[ids], "nodes":[keys], "params":{…}}
  is_override   boolean NOT NULL DEFAULT false,
  PRIMARY KEY (run_id, node_key)
);

CREATE TABLE model.node_overrides (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  model_id    bigint NOT NULL,
  node_key    text NOT NULL,
  as_of       date NOT NULL,
  value_num   double precision, value_bool boolean,
  reason      text NOT NULL,
  actor_id    bigint NOT NULL,
  valid_until date,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE model.allocations (
  run_id            bigint NOT NULL REFERENCES model.model_runs(id),
  risk_profile_id   bigint NOT NULL REFERENCES model.risk_profiles(id),
  line_item         text NOT NULL,          -- entity key: 'eq.large', 'sector.capgoods', 'metal.gold'
  group_key         text NOT NULL,          -- 'asset.equity'
  group_weight      numeric(9,6) NOT NULL,
  within_group      numeric(9,6) NOT NULL,
  portfolio_weight  numeric(9,6) NOT NULL,
  neutral_weight    numeric(9,6) NOT NULL,
  PRIMARY KEY (run_id, risk_profile_id, line_item)
);
```

**jobs**

```sql
CREATE TABLE jobs.job_schedules (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name            text UNIQUE NOT NULL,
  kind            text NOT NULL,
  args            jsonb NOT NULL DEFAULT '{}',
  schedule_type   text NOT NULL CHECK (schedule_type IN ('cron','interval','calendar')),
  expr            text NOT NULL,            -- '0 7 * * *' | '6h' | 'business_day:10' | 'month_day:12' | 'review:quarterly'
  timezone        text NOT NULL DEFAULT 'Asia/Kolkata',
  overlap_policy  text NOT NULL DEFAULT 'skip' CHECK (overlap_policy IN ('skip','queue','cancel_previous')),
  catchup_policy  text NOT NULL DEFAULT 'latest' CHECK (catchup_policy IN ('none','latest','all')),
  jitter_secs     int NOT NULL DEFAULT 0,
  enabled         boolean NOT NULL DEFAULT true,
  next_run_at     timestamptz NOT NULL,
  last_run_id     bigint,
  created_by      bigint, updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sched_due ON jobs.job_schedules (next_run_at) WHERE enabled;

CREATE TABLE jobs.job_runs (
  id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid             uuid UNIQUE NOT NULL DEFAULT gen_random_uuid(),
  kind             text NOT NULL,
  args             jsonb NOT NULL,
  args_version     int NOT NULL,
  trigger          text NOT NULL CHECK (trigger IN ('schedule','event','user','chain','system')),
  schedule_id      bigint, user_id bigint, parent_id bigint, root_id bigint,
  idempotency_key  text,
  queue            text NOT NULL,
  status           text NOT NULL CHECK (status IN ('queued','running','succeeded','failed','cancelled','skipped')),
  attempt          int NOT NULL DEFAULT 0,
  progress         smallint NOT NULL DEFAULT 0,
  message          text,
  result           jsonb,
  result_uri       text,
  error            jsonb,                    -- {"code","message","retryable"}
  engine_job_id    bigint,
  cancel_requested boolean NOT NULL DEFAULT false,
  queued_at        timestamptz NOT NULL DEFAULT now(),
  started_at       timestamptz, finished_at timestamptz,
  expires_at       timestamptz
);
CREATE UNIQUE INDEX job_idem ON jobs.job_runs (user_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX job_root ON jobs.job_runs (root_id);
CREATE INDEX job_kind_status ON jobs.job_runs (kind, status, queued_at DESC);
```

**auth + audit**

```sql
CREATE TABLE auth.users (
  id     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  email  text UNIQUE NOT NULL,
  name   text NOT NULL,
  role   text NOT NULL CHECK (role IN ('viewer','analyst','admin'))
);
CREATE TABLE auth.api_keys (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id bigint NOT NULL REFERENCES auth.users(id),
  hash bytea NOT NULL, label text, created_at timestamptz NOT NULL DEFAULT now(), revoked_at timestamptz
);
CREATE TABLE auth.audit_log (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  actor_id   bigint,
  entity     text NOT NULL,     -- 'series', 'model_config', 'job_schedule', 'datapoint', 'model_run'
  entity_id  text NOT NULL,
  action     text NOT NULL,     -- create | update | retract | approve | publish | enable | disable
  before     jsonb, after jsonb,
  at         timestamptz NOT NULL DEFAULT now()
);
```

**What lives where**

| Stored in DB (durable) | Carried in job args (transient) |
| --- | --- |
| Datapoints, raw payload index, series, entities, configs | Which connector, series and date range to fetch |
| Every run, run value, allocation | Which as-of date and config to compute |
| Job runs, progress, small results; large results as blob URIs | Retry state, attempt count (engine-managed) |
| Schedules, overrides, audit | Nothing that cannot be recomputed |

## Datapoints & derivations

Every value enters through one writer and is read through one point-in-time reader. Derived values are materialized by a cascade job, so model runs read fixed, traceable numbers.

**Writer and reader interfaces**

```go
package datapoints

type Point struct {
    SeriesKey  string
    ObsDate    civil.Date
    Num        *float64
    Bool       *bool
    Cat        *string
    KnownAt    time.Time        // zero = now; computed rows pass max(inputs.known_at)
    Note       string
    Inputs     []int64          // computed only
    DerivVer   int              // computed only
}

type WriteMeta struct {
    Source   string            // 'fred', 'analyst', 'override', 'computed', …
    ActorID  *int64
    JobRunID *int64
}

type WriteResult struct {
    BatchID  uuid.UUID
    Written  int
    Skipped  int               // identical to current resolved value / same input_hash
    Rejected []Rejection       // validation failures, per point
}

type Writer interface {
    // Validates against series.validation, skips no-ops, inserts rows,
    // inserts one outbox event, all in tx.
    Write(ctx context.Context, tx pgx.Tx, meta WriteMeta, pts []Point) (WriteResult, error)
    Retract(ctx context.Context, tx pgx.Tx, meta WriteMeta, ids []int64, reason string) error
}

type ReadOpts struct {
    Cutoff  time.Time   // knowledge cutoff; zero = now
    Sources []string    // optional: restrict; default = series.source_priority
}

type Reader interface {
    At(ctx context.Context, series string, d civil.Date, o ReadOpts) (*Resolved, error)
    Range(ctx context.Context, series string, from, to civil.Date, o ReadOpts) ([]Resolved, error)
    History(ctx context.Context, series string, upTo civil.Date, window Months, o ReadOpts) ([]Resolved, error)
    Versions(ctx context.Context, series string, d civil.Date) ([]Datapoint, error) // all sources, all revisions
}
```

**Point-in-time resolution**

For each `(series, obs_date)`: keep active rows with `known_at <= cutoff`, take the latest per source, then pick the first source in `series.source_priority`.

```sql
-- name: ResolveRange :many
WITH latest AS (
  SELECT DISTINCT ON (d.obs_date, d.source_id) d.*, s.key AS source_key
  FROM dp.datapoints d JOIN catalog.sources s ON s.id = d.source_id
  WHERE d.series_id = $1 AND d.obs_date BETWEEN $2 AND $3
    AND d.known_at <= $4 AND d.status = 'active'
  ORDER BY d.obs_date, d.source_id, d.known_at DESC
)
SELECT DISTINCT ON (obs_date) *
FROM latest
ORDER BY obs_date, array_position($5::text[], source_key);
```

A retraction is a row with `status = 'retracted'` and `supersedes_id`; it hides the superseded row for cutoffs after its `known_at`.

**Derivation definition** (stored in `series.derivation`)

```json
{
  "fn": "percentile_rank",
  "inputs": {"x": "spread.mid_large"},
  "params": {"window_months": 180, "min_obs": 24},
  "version": 2
}
```

```json
{
  "fn": "expr",
  "inputs": {"pe": "pe.nifty50", "y": "yield.gsec10y"},
  "params": {"expr": "100 / pe - y"},
  "version": 1
}
```

```json
{
  "fn": "rubric",
  "inputs": {"stance": "judg.rbi.stance", "growth": "judg.growth_vs_exp", "infl": "judg.inflation_trend"},
  "params": {
    "base": 50, "clamp": [0, 100],
    "add": {
      "stance": {"cutting": 20, "hold": 0, "hiking": -20},
      "growth": {"beat": 10, "in_line": 0, "miss": -10},
      "infl":   {"falling": 5, "stable": 0, "rising": -5}
    }
  },
  "version": 1
}
```

**Primitive registry**

```go
package derive

type Inputs map[string][]datapoints.Resolved   // alias → history up to obs_date

type Primitive interface {
    Name() string
    ValidateParams(p json.RawMessage) error
    Lookback(p json.RawMessage) Months           // how much history it needs
    Compute(ctx context.Context, d civil.Date, in Inputs, p json.RawMessage) (Value, error)
}

func Register(p Primitive)
```

| Primitive | Does | Used for |
| --- | --- | --- |
| `expr` | Same-date arithmetic via expr-lang | Spreads, AAA bps, gold/silver ratio, earnings-yield gap, real yield, breadth |
| `rolling_mean` | Mean over N periods | 3M avg flows |
| `ratio_of` | a ÷ b with alignment | Flows % AUM |
| `pct_change` | Change over N months | Gold 12M return, 6M gold move |
| `diff_over` | Level change over N months | TIPS 6M change |
| `percentile_rank` | Rank of value at d among history ≤ d in window; null below `min_obs` | All %ile series |
| `invert` | 100 − x | Valuation and flow scores |
| `avg` | Mean of inputs, ignoring nulls | Two-part signals |
| `rubric` | Base + lookup adds, clamp | Macro, credit, fundamentals rubrics |
| `bucket_map` | Range or category → fixed score per bucket | Rate-cycle table, TIPS table, PMI table, EPS gap table |
| `const` | Fixed value | Liquid = 90, non-driver = 50 |
| `consecutive` | True if condition holds N periods | Flow froth (top decile 3+ months) |
| `threshold` | True if x op k | Parabolic (>40% in 6M), EPS cut >15% |
| `fallback` | First non-null input, else default 50 | "Missing signal scores 50" |

**Cascade algorithm** (`derive.cascade`)

1. Read the outbox event: `series_ids`, `min_date`, `max_date`.
2. Collect all downstream series through `series_deps`; topologically sort them; detect cycles at series-save time, not here.
3. For each series, compute the affected range: same-date layers use `[min, max]`; windowed layers extend to `[min, max + lookback]` capped at today.
4. For each date: load inputs through the reader at cutoff = now, compute, hash input datapoint ids, skip if hash equals the current row's.
5. Write all results in one batch per layer with source `computed`, `known_at = max(inputs.known_at)`.
6. The writer's own outbox event is marked `origin = cascade` and is not re-cascaded; it notifies subscribers such as `veto.evaluate`.

**Percentile recompute ranges**

| Change | Dates recomputed |
| --- | --- |
| New latest value at d | `[d, d]` |
| Revision or backfill at d | `[d, d + window]` |
| Derivation version change | Whole series, via `derive.cascade` in rebuild mode |

**Seed series from the workbook** (abridged; full list ships in `configs/seed/series.yaml`)

| Key | Layer | Source / derivation |
| --- | --- | --- |
| `pe.nifty50`, `pe.nifty100`, `pe.midcap150`, `pe.smallcap250` | raw | NSE |
| `yield.gsec10y`, `rate.repo`, `yield.aaa3y`, `yield.tbill1y` | raw | RBI/CCIL, FIMMDA |
| `cpi.yoy`, `cpi.index` | raw | MOSPI |
| `tips.10y` | raw | FRED `DFII10` |
| `gold.inr10g`, `silver.inrkg` | raw | Bullion |
| `flows.{large,mid,small,sectoral,gold_etf}`, `aum.{…}` | raw | AMFI |
| `breadth.nifty100.up`, `.down` | raw | Analyst (no free feed) |
| `judg.rbi.stance`, `judg.growth_vs_exp`, `judg.inflation_trend`, `judg.credit_cycle`, `judg.rbi_path` | judgment | Analyst |
| `spread.mid_large`, `spread.small_large` | derived | `expr: mid − large` |
| `spread.aaa_bps` | derived | `expr: (aaa3y − gsec10y) × 100` |
| `ratio.gold_silver` | derived | `expr: gold × 100 / silver` |
| `gap.earnings_yield` | derived | `expr: 100 / pe − y` |
| `ret.gold.12m` | window | `pct_change 12` |
| `flows.small.3m_pct_aum` | window | `rolling_mean 3` ÷ AUM |
| `pctile.180m.*` | percentile | `percentile_rank` |
| `score.l1.{equity,debt,metals}.{valuation,macro,fundamentals,flows,momentum}` | score | invert / as-is / rubric |
| `score.eq.*`, `score.debt.*`, `score.metal.*`, `score.sector.*` | score | Per Calculation Guide |
| `trig.flow_froth.*`, `trig.parabolic.*`, `trig.eps_collapse.*` | trigger | `consecutive`, `threshold` |
| `trig.currency_stress`, `trig.credit_event`, `trig.amc_restrictions.*` | trigger | Analyst |

## Job system

Job kinds live in code; triggers live in data. Any kind can be started by any trigger it allows, and only `internal/jobs/river` imports the queue engine, so swapping engines touches one package.

**Core interfaces**

```go
package jobs

type Args interface{ Kind() string }   // JSON-serializable; add fields, never rename

type TriggerType string
const (
    TriggerSchedule TriggerType = "schedule"
    TriggerEvent    TriggerType = "event"
    TriggerUser     TriggerType = "user"
    TriggerChain    TriggerType = "chain"
    TriggerSystem   TriggerType = "system"
)

type UniqueOpts struct {
    ByArgs   []string       // arg fields that form the key; empty = all
    Period   time.Duration  // dedupe window
    States   []string       // default: queued, running
}

type Spec struct {
    Kind            string
    Queue           string          // ingest | compute | model | user | system
    Triggers        []TriggerType
    UserTriggerable bool
    Roles           []string        // who may start it via API
    MaxAttempts     int
    Timeout         time.Duration
    Backoff         func(attempt int) time.Duration
    Unique          *UniqueOpts
    Concurrency     int             // global cap; per-user cap on the user queue
    ArgsVersion     int
    ArgsSchema      json.RawMessage // JSON Schema, served by /v1/job-kinds
    ResultTTL       time.Duration
}

type Outcome struct {
    Summary map[string]any          // stored in job_runs.result
    BlobURI string                  // large result
    Next    []Args                  // chained follow-ups (trigger = chain)
}

type RunCtx struct {
    RunID, RootID int64
    Trigger       TriggerType
    UserID        *int64
    Log           *slog.Logger
    Progress      func(pct int, msg string)   // throttled writes to job_runs
    Cancelled     func() bool                 // also ctx.Done()
    DP            datapoints.Writer           // auto-tags job_run_id
}

type Handler[A Args] interface {
    Handle(ctx context.Context, rc *RunCtx, args A) (Outcome, error)
}

type Enqueuer interface {
    Enqueue(ctx context.Context, a Args, o ...Opt) (RunRef, error)
    EnqueueTx(ctx context.Context, tx pgx.Tx, a Args, o ...Opt) (RunRef, error)
}
// Opt: WithTrigger, WithUser, WithParent, WithSchedule, WithIdempotencyKey, WithRunAt, WithPriority

func Register[A Args](r *Registry, s Spec, h Handler[A])

// Error helpers for handlers
func Retryable(err error) error   // engine retries with backoff
func Permanent(err error) error   // fail now, no retry
func Snooze(d time.Duration) error // re-run later without counting an attempt (source not published yet)
```

**Middleware chain** (wraps every handler, outer to inner)

1. Recover panics → `Permanent`.
2. Create or update the `job_runs` row: status, attempt, timestamps.
3. Tracing span + Prometheus metrics (duration, outcome by kind).
4. Timeout from `Spec.Timeout`.
5. Cancellation watcher: polls `cancel_requested` every 2s and cancels ctx.
6. Rate limiter per connector (ingest queue only).
7. On success: persist `Outcome`, enqueue `Next` in the same tx with `trigger = chain`, `parent_id`, `root_id`.

**Triggers**

| Trigger | Started by | How |
| --- | --- | --- |
| schedule | `job_schedules` row | `scheduler.tick` (River periodic, every minute) selects due rows `FOR UPDATE SKIP LOCKED`, enqueues with unique key `(schedule_id, slot)`, applies overlap and catch-up policy, sets `next_run_at` |
| event | Outbox row | `outbox.dispatch` (every 5s, or LISTEN/NOTIFY) maps topics to subscribers; each subscriber returns `[]Args` |
| user | `POST /v1/jobs` | Checks `UserTriggerable` + role, idempotency key, per-user concurrency |
| chain | Parent `Outcome.Next` | Same tx as parent completion |
| system | Internal | Retries, cleanup, backfills fanning out |

**Event subscribers** (registry, same pattern)

```go
type Subscriber interface {
    Topic() string                                  // 'datapoints.written', 'run.published'
    Handle(ctx context.Context, ev Event) ([]Args, error)
}
```

| Topic | Subscriber | Enqueues |
| --- | --- | --- |
| `datapoints.written` (origin ≠ cascade) | CascadeSubscriber | `derive.cascade{batch_id}`, unique by batch, 3s debounce |
| `datapoints.written` (layer = trigger) | VetoSubscriber | `veto.evaluate{as_of}` |
| `run.completed` | — | MVP: none (hook for notifications) |
| `job.failed` | — | MVP: logged + metric by middleware; an alert job comes later |

**Calendar schedule expressions**

| Expr | Meaning | Example use |
| --- | --- | --- |
| `0 7 * * *` (cron) | Standard cron in schedule timezone | Daily FRED, bullion |
| `6h` (interval) | Every 6 hours from creation | Quality checks |
| `business_day:10` | 10th NSE business day of each month | AMFI flows |
| `month_day:12` | 12th of month, or next business day | CPI |
| `month_end+3bd` | 3 business days after month end | NSE index P/E |
| `review:quarterly` | Dates listed in model config `review_calendar` | Satellite review run |
| `review:semiannual` | Same | Full strategic run |

Calendar types are themselves a registry (`ScheduleType` interface with `Next(after time.Time) time.Time`), so new calendars need no scheduler change.

**Job catalogue (MVP)**

The MVP builds six feature job types: the five below plus \`backtest.run\` (specified under Dashboard support). None of the first five needs an external data source (\`backtest.run\` needs fund NAVs and index levels): data enters through `import.file` and analyst entry, and the other four compute from what is stored. Two infrastructure jobs run the machinery and are not features.

| Kind | Queue | Triggers | Who can start | Args | Unique |
| --- | --- | --- | --- | --- | --- |
| `import.file` | ingest | user, schedule (watch folder later) | analyst | `{upload_id, mapping_id, dry_run}` | upload |
| `derive.cascade` | compute | event, user | admin (rebuild) | `{batch_id}` or `{mode:"rebuild", series[], include_downstream}` | batch / series set |
| `veto.evaluate` | model | event, schedule, user | analyst | `{as_of}` | as\_of, 10m |
| `model.run` | model | schedule, user | analyst | `{model, as_of, cutoff?, config_version?, params, publish}` | model + as\_of + config |
| `quality.check` | compute | schedule, user | analyst | `{as_of?, series[]?, checks[]?}` | 1 at a time |
| `scheduler.tick` (infra) | system | system | — | `{}` | 1 per minute |
| `outbox.dispatch` (infra) | system | system | — | `{}` | 1 at a time |

**1. `import.file`** — the stand-in for every connector

- Input: an uploaded CSV or XLSX plus a saved mapping (column → series, date format, `known_at` rule).
- Steps: parse → map columns → validate per series → `dry_run` returns a preview and stops → else write one batch through the writer.
- Output: `{rows_written, rows_skipped, rejected[], batch_id}`; the write's outbox event starts `derive.cascade`.
- Failure: a bad row is rejected and reported, never fatal; a bad mapping fails the job `Permanent`.
- Used for: seeding 5–10 years of tracker history now, and the monthly tracker upload until connectors exist.

**2. `derive.cascade`** — recompute everything downstream

- Incremental mode `{batch_id}`: from an outbox event; walks `series_deps`, topo-sorts, recomputes the affected date ranges (see Datapoints & derivations).
- Rebuild mode `{mode:"rebuild", series[]}`: after a derivation version change or a new series; recomputes full history.
- Output: `{series, rows_written, rows_skipped}`; writes with `origin = cascade` so it never re-triggers itself.
- Failure: retryable; idempotent through `input_hash`.

**3. `veto.evaluate`** — turn trigger datapoints into veto states

- Reads all `trig.*` datapoints at `as_of` (computed ones like flow froth, and analyst ones like currency stress).
- Maps triggers to buckets using the active config's `vetoes` map; writes `veto.*` datapoints (bool, computed) with lineage to the triggers.
- Output: `{as_of, active: ["eq.small"]}`; a newly active veto is logged for the analyst.

**4. `model.run`** — produce the allocation

- Loads config (active, or a draft for what-if), reads score and veto datapoints point-in-time at `cutoff`, runs the step pipeline, writes `model_runs`, `run_values`, `allocations`.
- `publish: true` publishes directly (admin only); otherwise the run is a draft awaiting approve and publish.
- Output: `{run_id, status, warnings}`. Missing scores use 50 and become warnings, not failures.
- Failure: invalid params or config → `Permanent` with pointer errors.

**5. `quality.check`** — the analyst's to-do list while data is manual

- Checks: stale series (no value for the expected period), missing scores at the next review date, insufficient history for percentiles, outliers (|z| > 4), category values outside the allowed set.
- Output: the same shape as `GET /v1/quality/report`; stored as the job result so the latest report is one query away.
- Example finding: `flows.small` has no October value, expected by 14 Nov → upload the tracker.

**Which datapoints each job touches**

Only `import.file` and analyst entry bring data in; the other jobs compute from stored datapoints.

| Job | Datapoints | Direction |
| --- | --- | --- |
| `import.file` | raw series from the monthly upload | writes |
| Analyst entry (API, not a job) | judgment series, analyst-only triggers, score overrides | writes |
| `derive.cascade` | derived → window → percentile → score → computed trigger series | reads raw + judgment, writes computed |
| `veto.evaluate` | reads `trig.*`, writes `veto.*` per bucket | reads + writes |
| `model.run` | reads `score.*` + `veto.*` | writes run values (not datapoints) |
| `quality.check` | reads every raw and judgment series, plus scores due at the next review | report only |

**Upload template (`import.file`)**

| Group | Series | Status |
| --- | --- | --- |
| Index valuations | Nifty 100, Midcap 150, Smallcap 250 P/E | In Data Trackers today |
| Rates | 10Y G-sec, repo, 3Y AAA | In Data Trackers today |
| Metals | Gold ₹/10g, silver ₹/kg | In Data Trackers today |
| Flows | AMFI large, mid, small, sectoral, gold ETF | In Data Trackers today |
| Equity extras | Nifty 50 P/E and TR index, S&P 500 fwd P/E, sector index P/E/P/B and returns | New columns |
| Macro | CPI YoY, CPI index, US TIPS 10Y, global PMI | New columns |
| Flows extras | Category AUMs (equity, duration, gold ETF) | New columns |
| Debt extras | Bond index 12M return, category YTMs, 1Y T-bill | New columns |
| Fundamentals | Segment consensus EPS growth, aggregate EBITDA margins | New columns |

**Analyst entry (`POST /v1/datapoints`)**

| Group | Series |
| --- | --- |
| Macro judgments | RBI stance, expected RBI path, growth vs expectations, inflation trend, fiscal supply risk |
| Credit judgments | Credit cycle, system liquidity, credit-fund stress |
| Sector and metals judgments | Sector cycle position, regulatory overhang, CB gold buying, gold ETF holdings trend, solar/EV news flow |
| Earnings | Revision breadth (upgrade and downgrade counts), results beats vs misses |
| Analyst triggers | Currency stress, credit event, AMC inflow restrictions |
| Overrides | Any score |

**Computed by `derive.cascade`** (never typed in): spreads, AAA bps, gold/silver ratio, earnings-yield gap, real yields, 3M flows, 12M returns, all percentiles, all 85 scores, and the computable triggers (flow froth, parabolic move, EPS collapse).

**One datapoint end to end**

1. The 10 Nov upload writes `flows.small` = ₹3,400 cr for 31 Oct (`import.file`).
2. `derive.cascade` recomputes `flows.small.3m_pct_aum` → `pctile.180m.flows.small` → `score.eq.eq.small.flows` and `trig.flow_froth.eq.small`.
3. The trigger changed, so `veto.evaluate` writes `veto.eq.small` = true.
4. On review day `model.run` reads the score and veto and caps Small Cap's tilt at zero.
5. Had no October value arrived by the due date, `quality.check` would flag `flows.small` as stale.

**Deferred until data sources are confirmed**

| Kind | Why later | Plugs in as |
| --- | --- | --- |
| `ingest.fetch` | Needs confirmed APIs for FRED, AMFI, RBI/CCIL, NSE, bullion | New kind + one connector per source; writes through the same writer |
| `ingest.backfill` | Same | Fans out into `ingest.fetch` |
| `allocation.publish` | Folded into `model.run` (`publish` flag) and `POST /v1/runs/{id}/publish` | Split out if publishing gains side effects |
| `alert.send` | Logs and metrics cover MVP | Event subscriber on `job.failed`, `veto.activated` |
| `maintenance.prune` | Little data to prune yet | Scheduled system job |

**Seed schedules**

| Name | Kind | Expr | Args |
| --- | --- | --- | --- |
| quality-daily | quality.check | `30 8 * * 1-5` | `{as_of:"$today"}` |
| data-due-monthly | quality.check | `business_day:12` | `{as_of:"$prev_month_end", checks:["stale","missing_scores"]}` |
| veto-monthly | veto.evaluate | `business_day:12` | `{as_of:"$prev_month_end"}` |
| review-quarterly | model.run | `review:quarterly` | `{model:"maaf", as_of:"$slot", publish:false}` |
| review-semiannual | model.run | `review:semiannual` | `{model:"maaf", as_of:"$slot", publish:false}` |

Arg templates such as `$today`, `$month_end` and `$slot` are resolved by the scheduler at enqueue time.

**Adding a job kind** (one file in `internal/jobs/kinds/`)

```go
type QualityCheckArgs struct {
    Series []string `json:"series,omitempty"`
    Checks []string `json:"checks,omitempty"`
}
func (QualityCheckArgs) Kind() string { return "quality.check" }

type qualityCheck struct{ cat catalog.Catalog; rd datapoints.Reader }

func (h *qualityCheck) Handle(ctx context.Context, rc *jobs.RunCtx, a QualityCheckArgs) (jobs.Outcome, error) {
    // … compute findings, rc.Progress(…)
    return jobs.Outcome{Summary: map[string]any{"stale": stale, "outliers": out}}, nil
}

func init() {
    jobs.Register(jobs.Default, jobs.Spec{
        Kind: "quality.check", Queue: "compute",
        Triggers: []jobs.TriggerType{jobs.TriggerSchedule, jobs.TriggerUser},
        UserTriggerable: true, Roles: []string{"analyst", "admin"},
        MaxAttempts: 3, Timeout: 5 * time.Minute, ArgsVersion: 1,
        Unique: &jobs.UniqueOpts{Period: 10 * time.Minute},
    }, &qualityCheck{ /* deps injected via kinds.Deps */ })
}
```

**Engine portability**

| Our concept | River | asynq (if ever) |
| --- | --- | --- |
| `Args.Kind()` | `JobArgs.Kind()` | Task type name |
| Args JSON | Job args JSON | Payload bytes |
| `EnqueueTx` | `InsertTx` | Outbox table + dispatcher (no native tx) |
| `Unique` | Unique opts | `asynq.Unique(ttl)` |
| `Snooze` | `JobSnooze` | Re-enqueue with `ProcessIn` |
| `job_runs` | Ours, unchanged | Ours, unchanged |

## Model engine

The model is a pipeline of registered steps reading score and trigger datapoints, driven entirely by a versioned config. Reproducing the workbook's numbers exactly (e.g. Debt L1 tilt 3.0%, final 35.6%) is the acceptance test.

**Pipeline**

```
load inputs → composite → veto → tilt → sector qualify → constraints → normalize → sleeve carve-out → rollup → glide path (later)
```

```go
package model

type State struct {
    AsOf    civil.Date
    Cutoff  time.Time
    Config  Config
    Params  Params
    Profile RiskProfile
    Values  map[string]Node          // node_key → value + inputs
    Warn    []Warning
}

type Step interface {
    Name() string
    Run(ctx context.Context, s *State) error
}

type Constraint interface {
    Name() string
    Apply(ctx context.Context, s *State, scorecard string, p json.RawMessage) error
}

func RegisterStep(s Step)
func RegisterConstraint(c Constraint)
```

The config lists step names in order, so a new step (an optimizer, a drawdown guard) is a registration plus a config change.

**Formulas**

Composite per bucket b over signals i:

```latex
C_b = \sum_i w_i \, s_{b,i}, \quad \sum_i w_i = 1
```

Tilt, with cap k for the scorecard and veto v (1 if active):

```latex
t_b = \begin{cases} \min\left(\frac{C_b - 50}{50} k,\ 0\right) & v_b = 1 \\ \frac{C_b - 50}{50} k & v_b = 0 \end{cases}
```

Preliminary and normalized weight, with neutral weight n:

```latex
p_b = n_b (1 + t_b), \qquad f_b = \frac{p_b}{\sum_j p_j}
```

Sector sleeve as share of equity, with cap S, held sectors h and max sectors m:

```latex
\text{sleeve} = S \cdot \frac{h}{m}, \qquad \text{core}_b = f_b (1 - \text{sleeve})
```

Portfolio weight = group weight × within-group weight. Check with the sheet: Debt C = 57.5, k = 20% → t = 3.0%; p = 35% × 1.03 = 36.05%; Large Cap core = 46.6% × 0.925 = 43.1%.

**Config v1** (abridged; full file `configs/seed/model_maaf_v1.yaml`)

```json
{
  "steps": ["composite", "veto", "tilt", "sector_qualify", "constraints", "normalize", "sleeve", "rollup"],
  "missing_score": 50,
  "scorecards": {
    "l1": {
      "buckets": ["asset.equity", "asset.debt", "asset.metals"],
      "tilt_cap": 0.20,
      "signals": [
        {"key": "valuation",    "weight": 0.30},
        {"key": "macro",        "weight": 0.20},
        {"key": "fundamentals", "weight": 0.20},
        {"key": "flows",        "weight": 0.15},
        {"key": "momentum",     "weight": 0.15}
      ],
      "score_series": "score.l1.{bucket}.{signal}",
      "vetoes": {"asset.equity": ["trig.flow_froth.equity"], "asset.metals": ["trig.parabolic.gold"]}
    },
    "equity": {
      "parent": "asset.equity",
      "buckets": ["eq.large", "eq.mid", "eq.small", "eq.intl"],
      "tilt_cap": 0.30,
      "signals": [
        {"key": "valuation", "weight": 0.30}, {"key": "relative_value", "weight": 0.20},
        {"key": "revisions", "weight": 0.20}, {"key": "eps_growth_diff", "weight": 0.15},
        {"key": "margin_cycle", "weight": 0.15}
      ],
      "score_series": "score.eq.{bucket}.{signal}",
      "vetoes": {
        "*": ["trig.flow_froth.{bucket}", "trig.amc_restrictions.{bucket}", "trig.eps_collapse.{bucket}"],
        "eq.intl": ["trig.currency_stress"]
      }
    },
    "debt":   {"parent": "asset.debt",   "buckets": ["debt.liquid", "debt.corporate", "debt.gilt"], "tilt_cap": 0.30, "signals": ["…"]},
    "metals": {"parent": "asset.metals", "buckets": ["metal.gold", "metal.silver"], "tilt_cap": 0.25, "signals": ["…"]}
  },
  "sector_satellite": {
    "parent": "asset.equity",
    "sectors": ["sector.banks", "sector.it", "sector.pharma", "sector.auto", "sector.capgoods", "sector.fmcg", "sector.energy", "sector.metals"],
    "signals": [
      {"key": "valuation", "weight": 0.30}, {"key": "revisions", "weight": 0.30},
      {"key": "rel_momentum", "weight": 0.25}, {"key": "cycle", "weight": 0.15}
    ],
    "sleeve_cap": 0.15,
    "max_sectors": 2,
    "qualify_threshold": 70,
    "max_quarters_held": 4
  },
  "constraints": [
    {"name": "max_multiple_of_neutral", "scorecard": "l1", "bucket": "asset.metals", "params": {"max": 2.0}},
    {"name": "extreme_full_cap", "params": {"high": 80, "low": 20, "consecutive_reviews": 2, "normal_cap_factor": 1.0}},
    {"name": "sector_max_tenure", "params": {"quarters": 4}}
  ],
  "review_calendar": {"quarterly": ["2026-12-31", "2027-03-31"], "semiannual": ["2026-12-31", "2027-06-30"]}
}
```

`normal_cap_factor` is 1.0 until the extreme-score rule is clarified (see open questions).

**Run lifecycle**

| Status | Meaning | Who moves it |
| --- | --- | --- |
| running | Job in progress | System |
| draft | Complete, not reviewed | System |
| approved | Analyst signed off | Analyst |
| published | Current allocation for profiles | Admin (or auto if `publish: true`) |
| superseded | A newer run was published | System |
| failed | Error; see job run | System |

**Validation rules in every run**

- Signal weights per scorecard sum to 1 (config save also checks).
- Neutral weights per group sum to 1.
- Sectors held ≤ qualifying count and ≤ max sectors; else run fails with `invalid_params`.
- Final weights per group sum to 1 within 1e-9; portfolio sums to 1.
- Any score missing → 50, recorded as a warning with the series key.

## API reference

All endpoints are JSON over HTTPS under `/v1`, authenticated with `Authorization: Bearer <api_key>`. Every mutation writes an audit row; long work returns a job run instead of blocking.

**Conventions**

| Topic | Rule |
| --- | --- |
| Dates | `obs_date`, `as_of`: `YYYY-MM-DD` (IST calendar). Timestamps: RFC 3339 UTC |
| Point-in-time | Any read accepts `?cutoff=<timestamp>`; default now |
| Pagination | `?limit=50&cursor=<opaque>`; response `{"data":[…],"next_cursor":"…"}` |
| Idempotency | `Idempotency-Key` header on POSTs that create work |
| Roles | viewer: GET · analyst: datapoints, imports, user jobs, approve runs · admin: catalog, configs, schedules, publish |
| Async | `202 Accepted` + `{"job_run":{…}}`; poll `GET /v1/jobs/{id}` or stream `GET /v1/jobs/{id}/events` (SSE) |

**Error shape**

```json
{
  "error": {
    "code": "validation_failed",
    "message": "2 of 3 points rejected",
    "details": [
      {"index": 1, "field": "value", "reason": "out_of_range", "min": 0, "max": 100}
    ],
    "request_id": "req_7f3a…"
  }
}
```

Codes: `unauthorized`, `forbidden`, `not_found`, `validation_failed`, `conflict`, `idempotency_mismatch`, `invalid_params`, `not_user_triggerable`, `rate_limited`, `internal`.

### Catalog

**Endpoints**

| Method | Path | Role |
| --- | --- | --- |
| GET | `/v1/entities?kind=sector` | viewer |
| POST / PATCH | `/v1/entities`, `/v1/entities/{key}` | admin |
| GET | `/v1/series?layer=score&entity=eq.mid&tag=l1` | viewer |
| GET | `/v1/series/{key}` | viewer |
| POST | `/v1/series` | admin |
| PATCH | `/v1/series/{key}` | admin |
| GET | `/v1/series/{key}/deps?direction=up\|down&depth=3` | viewer |
| GET | `/v1/sources` | viewer |

`POST /v1/series` — create a derived percentile series

```json
{
  "key": "pctile.180m.spread.mid_large",
  "name": "Mid–Large P/E spread percentile (15Y)",
  "entity": "eq.mid",
  "layer": "percentile",
  "value_type": "num",
  "unit": "pctile",
  "frequency": "monthly",
  "validation": {"min": 0, "max": 100},
  "min_history": 24,
  "derivation": {
    "fn": "percentile_rank",
    "inputs": {"x": "spread.mid_large"},
    "params": {"window_months": 180}
  },
  "tags": ["data_trackers"]
}
```

`201 Created`

```json
{
  "key": "pctile.180m.spread.mid_large",
  "layer": "percentile",
  "derivation_ver": 1,
  "deps": ["spread.mid_large"],
  "rebuild_job": {"id": "a1c2…", "kind": "derive.cascade", "args": {"mode": "rebuild", "series": ["pctile.180m.spread.mid_large"]}, "status": "queued"}
}
```

Saving a derivation validates params, rejects cycles (`409 conflict`), rebuilds `series_deps`, and enqueues `derive.cascade` in rebuild mode for history.

### Datapoints

**Endpoints**

| Method | Path | Role |
| --- | --- | --- |
| POST | `/v1/datapoints` | analyst |
| GET | `/v1/series/{key}/datapoints?from=&to=&cutoff=` | viewer |
| GET | `/v1/series/{key}/datapoints/{obs_date}` | viewer |
| GET | `/v1/series/{key}/datapoints/{obs_date}/versions` | viewer |
| GET | `/v1/datapoints/{id}/lineage?depth=5` | viewer |
| POST | `/v1/datapoints/{id}/retract` | analyst |
| POST | `/v1/snapshot` | viewer |

`POST /v1/datapoints` — analyst input or override (any number of points, one batch)

```json
{
  "source": "analyst",
  "points": [
    {"series": "judg.rbi.stance", "obs_date": "2026-10-31", "value": "cutting", "note": "Oct MPC cut 25bp, guidance dovish"},
    {"series": "breadth.nifty100.up", "obs_date": "2026-10-31", "value": 48, "note": "Broker X strategy note p.4"},
    {"series": "score.eq.eq.large.relative_value", "obs_date": "2026-10-31", "value": 60, "note": "Override: spread data incomplete"}
  ]
}
```

`source` is `analyst` or `override` (admin for override). Values are typed by the series: number, boolean or allowed category.

`201 Created`

```json
{
  "batch_id": "5b0e…",
  "written": 3,
  "skipped": 0,
  "rejected": [],
  "cascade_job": {"id": "c91f…", "kind": "derive.cascade", "status": "queued"}
}
```

`GET /v1/series/pctile.180m.spread.mid_large/datapoints?from=2026-01-01&to=2026-03-31&cutoff=2026-02-15T00:00:00Z`

```json
{
  "series": "pctile.180m.spread.mid_large",
  "cutoff": "2026-02-15T00:00:00Z",
  "data": [
    {"id": 9012, "obs_date": "2026-01-31", "value": 71.4, "source": "computed", "known_at": "2026-02-04T12:30:00Z", "derivation_ver": 1},
    {"id": 9155, "obs_date": "2026-02-28", "value": null, "source": null, "known_at": null, "reason": "not_known_at_cutoff"}
  ]
}
```

`GET /v1/series/yield.gsec10y/datapoints/2026-10-31/versions`

```json
{
  "resolved_id": 30411,
  "versions": [
    {"id": 30411, "value": 6.70, "source": "override", "known_at": "2026-11-01T09:10:00Z", "actor": "riya@…", "note": "RBI print revised", "status": "active"},
    {"id": 30122, "value": 6.72, "source": "rbi", "known_at": "2026-10-31T13:45:00Z", "status": "active"}
  ]
}
```

`GET /v1/datapoints/41877/lineage?depth=3`

```json
{
  "id": 41877,
  "series": "score.l1.equity.valuation",
  "obs_date": "2026-10-31",
  "value": 38.0,
  "derivation": {"fn": "avg", "version": 1},
  "inputs": [
    {"id": 41850, "series": "pctile.180m.gap.earnings_yield", "value": 30.0,
     "inputs": [{"id": 41790, "series": "gap.earnings_yield", "value": -2.2}]},
    {"id": 41851, "series": "score.l1.equity.pe_inverted", "value": 45.0,
     "inputs": [{"id": 41801, "series": "pctile.180m.pe.nifty50", "value": 55.0}]}
  ]
}
```

`POST /v1/datapoints/30411/retract`

```json
{"reason": "Entered against wrong month"}
```

`POST /v1/snapshot` — many series at one date, as the model sees them

```json
{"as_of": "2026-10-31", "cutoff": "2026-11-01T00:00:00Z", "series": ["score.l1.*", "trig.*"]}
```

```json
{
  "as_of": "2026-10-31",
  "values": {
    "score.l1.equity.valuation": {"value": 45, "datapoint_id": 41877, "source": "computed"},
    "trig.flow_froth.eq.small": {"value": true, "datapoint_id": 41920, "source": "computed"}
  },
  "missing": ["score.l1.metals.fundamentals"]
}
```

### File imports (seeding history)

| Method | Path | Role |
| --- | --- | --- |
| POST | `/v1/uploads` (multipart: CSV or XLSX) | analyst |
| POST | `/v1/import-mappings` | analyst |
| POST | `/v1/jobs` with kind `import.file` | analyst |

`POST /v1/import-mappings` — maps the Data Trackers rates log

```json
{
  "name": "Data Trackers – rates log",
  "sheet": "Data Trackers",
  "header_row_match": "Month",
  "date_column": {"name": "Month", "format": "Jan-2006", "obs_date": "month_end"},
  "columns": [
    {"name": "10Y G-sec Yield %", "series": "yield.gsec10y"},
    {"name": "Repo Rate %", "series": "rate.repo"},
    {"name": "3Y AAA Corp Yield %", "series": "yield.aaa3y"}
  ],
  "ignore_columns": ["AAA Spread (bps)", "10Y Yield %ile", "Spread %ile"],
  "known_at": "obs_date_plus_days:5"
}
```

Derived and percentile columns are ignored on import; the system recomputes them point-in-time. `known_at` for historical imports is estimated from the publication lag so backtests stay honest.

### Jobs

| Method | Path | Role |
| --- | --- | --- |
| GET | `/v1/job-kinds` | viewer |
| POST | `/v1/jobs` | per kind |
| GET | `/v1/jobs?kind=&status=&trigger=&root_id=&since=` | viewer |
| GET | `/v1/jobs/{id}` | viewer |
| GET | `/v1/jobs/{id}/events` (SSE) | viewer |
| GET | `/v1/jobs/{id}/tree` | viewer |
| POST | `/v1/jobs/{id}/cancel` | owner or admin |
| POST | `/v1/jobs/{id}/retry` | admin |

`GET /v1/job-kinds`

```json
{
  "data": [
    {
      "kind": "model.run",
      "queue": "model",
      "user_triggerable": true,
      "roles": ["analyst", "admin"],
      "args_version": 1,
      "args_schema": {
        "type": "object",
        "required": ["model", "as_of"],
        "properties": {
          "model": {"type": "string"},
          "as_of": {"type": "string", "format": "date"},
          "cutoff": {"type": "string", "format": "date-time"},
          "config_version": {"type": ["integer", "string"]},
          "params": {"type": "object"},
          "publish": {"type": "boolean", "default": false}
        }
      }
    }
  ]
}
```

`POST /v1/jobs` (header `Idempotency-Key: 2f6c…`)

```json
{
  "kind": "model.run",
  "args": {
    "model": "maaf",
    "as_of": "2026-10-31",
    "config_version": "active",
    "params": {"sectors_held": ["sector.capgoods"]},
    "publish": false
  }
}
```

`202 Accepted`

```json
{
  "job_run": {
    "id": "8d2e6a1c-…",
    "kind": "model.run",
    "status": "queued",
    "trigger": "user",
    "queue": "model",
    "progress": 0,
    "queued_at": "2026-11-01T12:40:02Z",
    "links": {"self": "/v1/jobs/8d2e6a1c-…", "events": "/v1/jobs/8d2e6a1c-…/events"}
  }
}
```

`GET /v1/jobs/{id}` (finished)

```json
{
  "id": "8d2e6a1c-…",
  "kind": "model.run",
  "args": {"model": "maaf", "as_of": "2026-10-31", "…": "…"},
  "status": "succeeded",
  "trigger": "user",
  "user": "riya@…",
  "attempt": 1,
  "progress": 100,
  "message": "8 steps complete",
  "result": {"run_id": "f04b…", "warnings": 1},
  "parent_id": null,
  "root_id": "8d2e6a1c-…",
  "started_at": "2026-11-01T12:40:03Z",
  "finished_at": "2026-11-01T12:40:05Z"
}
```

SSE events from `/events`:

```
event: progress
data: {"progress": 40, "message": "tilt: equity scorecard"}

event: status
data: {"status": "succeeded", "result": {"run_id": "f04b…"}}
```

`GET /v1/jobs/{id}/tree` — what one daily ingest caused

```json
{
  "id": "11aa…", "kind": "import.file", "trigger": "user", "status": "succeeded", "result": {"rows_written": 15},
  "children": [
    {"id": "22bb…", "kind": "derive.cascade", "trigger": "event", "status": "succeeded", "result": {"series": 41, "rows_written": 47},
     "children": [{"id": "33cc…", "kind": "veto.evaluate", "trigger": "event", "status": "succeeded", "result": {"active": ["eq.small"]}}]}
  ]
}
```

Other user-triggered payloads:

```json
{"kind": "quality.check", "args": {"as_of": "2026-11-01", "checks": ["stale", "missing_scores"]}}
```

```json
{"kind": "veto.evaluate", "args": {"as_of": "2026-10-31"}}
```

```json
{"kind": "import.file", "args": {"upload_id": "up_91c…", "mapping_id": "map_rates", "dry_run": true}}
```

```json
{"kind": "derive.cascade", "args": {"mode": "rebuild", "series": ["pctile.180m.*"], "include_downstream": true}}
```

### Schedules

| Method | Path | Role |
| --- | --- | --- |
| GET | `/v1/schedules` | viewer |
| POST | `/v1/schedules` | admin |
| PATCH | `/v1/schedules/{id}` | admin |
| POST | `/v1/schedules/{id}/run-now` | admin |
| POST | `/v1/schedules/{id}/pause`, `/resume` | admin |
| GET | `/v1/schedules/{id}/preview?n=5` | viewer |

`POST /v1/schedules`

```json
{
  "name": "data-due-monthly",
  "kind": "quality.check",
  "args": {"as_of": "$prev_month_end", "checks": ["stale", "missing_scores"]},
  "schedule_type": "calendar",
  "expr": "business_day:12",
  "timezone": "Asia/Kolkata",
  "overlap_policy": "skip",
  "catchup_policy": "latest",
  "jitter_secs": 120,
  "enabled": true
}
```

`201 Created` returns the row plus `next_run_at`. `GET …/preview?n=3`:

```json
{"next": ["2026-11-14T09:00:00+05:30", "2026-12-14T09:00:00+05:30", "2027-01-14T09:00:00+05:30"]}
```

### Model configs

| Method | Path | Role |
| --- | --- | --- |
| GET | `/v1/models/{model}/configs` | viewer |
| GET | `/v1/models/{model}/configs/{version}` | viewer |
| POST | `/v1/models/{model}/configs` (creates draft) | admin |
| POST | `/v1/models/{model}/configs/{version}/validate` | admin |
| POST | `/v1/models/{model}/configs/{version}/activate` | admin |
| GET | `/v1/models/{model}/configs/diff?from=1&to=2` | viewer |

`POST /v1/models/maaf/configs`

```json
{
  "base_version": 1,
  "patch": [
    {"op": "replace", "path": "/scorecards/l1/tilt_cap", "value": 0.15},
    {"op": "replace", "path": "/sector_satellite/qualify_threshold", "value": 72}
  ],
  "note": "Tighter L1 cap after annual review"
}
```

`201 Created`

```json
{"version": 2, "status": "draft", "validation": {"ok": true, "errors": []}}
```

Validation errors use the error shape with JSON pointers, e.g. `{"path": "/scorecards/equity/signals", "reason": "weights_sum", "got": 0.95}`. A draft can be used in `model.run` with `config_version: 2` and `publish: false` for what-if runs.

### Runs and allocations

| Method | Path | Role |
| --- | --- | --- |
| GET | `/v1/runs?model=maaf&status=published&from=` | viewer |
| GET | `/v1/runs/{id}` | viewer |
| GET | `/v1/runs/{id}/values?step=tilt&scorecard=equity` | viewer |
| GET | `/v1/runs/{id}/values/{node_key}/lineage` | viewer |
| GET | `/v1/runs/{id}/allocations?profile=balanced` | viewer |
| GET | `/v1/runs/compare?a={id}&b={id}` | viewer |
| POST | `/v1/runs/{id}/approve` | analyst |
| POST | `/v1/runs/{id}/publish` | admin |
| GET | `/v1/allocations/current?model=maaf&profile=balanced` | viewer |
| POST / DELETE | `/v1/models/{model}/overrides`, `/{id}` | admin |

`GET /v1/runs/{id}`

```json
{
  "id": "f04b…",
  "model": "maaf",
  "config_version": 1,
  "as_of": "2026-10-31",
  "knowledge_cutoff": "2026-11-01T12:40:03Z",
  "params": {"sectors_held": ["sector.capgoods"]},
  "status": "draft",
  "warnings": [{"code": "missing_score", "series": "score.metal.silver.industrial", "used": 50}],
  "summary": {
    "l1": [
      {"bucket": "asset.equity", "composite": 50.5, "veto": false, "tilt": 0.002, "final": 0.545, "neutral": 0.55},
      {"bucket": "asset.debt",   "composite": 57.5, "veto": false, "tilt": 0.030, "final": 0.356, "neutral": 0.35},
      {"bucket": "asset.metals", "composite": 51.3, "veto": false, "tilt": 0.005, "final": 0.099, "neutral": 0.10}
    ],
    "sector_qualifiers": [{"sector": "sector.capgoods", "composite": 71.0}]
  },
  "job_run_id": "8d2e6a1c-…"
}
```

`GET /v1/runs/{id}/allocations?profile=balanced`

```json
{
  "run_id": "f04b…",
  "profile": "balanced",
  "lines": [
    {"line_item": "eq.large",        "group": "asset.equity", "group_weight": 0.545, "within_group": 0.431, "portfolio_weight": 0.235, "neutral": 0.2475},
    {"line_item": "eq.mid",          "group": "asset.equity", "group_weight": 0.545, "within_group": 0.179, "portfolio_weight": 0.098, "neutral": 0.11},
    {"line_item": "sector.capgoods", "group": "asset.equity", "group_weight": 0.545, "within_group": 0.075, "portfolio_weight": 0.041, "neutral": 0.0},
    {"line_item": "debt.gilt",       "group": "asset.debt",   "group_weight": 0.356, "within_group": 0.260, "portfolio_weight": 0.093, "neutral": 0.0875},
    {"line_item": "metal.gold",      "group": "asset.metals", "group_weight": 0.099, "within_group": 0.745, "portfolio_weight": 0.074, "neutral": 0.075}
  ],
  "total": 1.0
}
```

`GET /v1/runs/{id}/values/eq.tilt.eq.small/lineage`

```json
{
  "node": "eq.tilt.eq.small",
  "value": -0.101,
  "step": "tilt",
  "formula": "min((C-50)/50*cap, 0) because veto",
  "inputs": {
    "nodes": {"eq.composite.eq.small": 33.3, "eq.veto.eq.small": true},
    "params": {"cap": 0.30},
    "datapoints": [{"id": 41920, "series": "trig.flow_froth.eq.small", "value": true}]
  }
}
```

`POST /v1/runs/{id}/approve`

```json
{"note": "Reviewed with Q3 notes; small-cap veto confirmed"}
```

`POST /v1/models/maaf/overrides` — pin a run node

```json
{"node_key": "eq.veto.eq.intl", "as_of": "2026-10-31", "value": true, "reason": "INR stress episode", "valid_until": "2027-01-31"}
```

### Risk profiles, quality, audit, health

| Method | Path | Role |
| --- | --- | --- |
| GET / POST / PATCH | `/v1/risk-profiles`, `/{key}` | viewer / admin |
| GET | `/v1/quality/report?as_of=` | viewer |
| GET | `/v1/audit?entity=&entity_id=&actor=&since=` | admin |
| GET | `/healthz`, `/readyz`, `/metrics` | none |

`POST /v1/risk-profiles`

```json
{
  "key": "balanced",
  "name": "Balanced",
  "neutral_weights": {
    "asset.equity": 0.55, "asset.debt": 0.35, "asset.metals": 0.10,
    "eq.large": 0.45, "eq.mid": 0.20, "eq.small": 0.15, "eq.intl": 0.20,
    "debt.liquid": 0.40, "debt.corporate": 0.35, "debt.gilt": 0.25,
    "metal.gold": 0.75, "metal.silver": 0.25
  }
}
```

`GET /v1/quality/report?as_of=2026-11-01`

```json
{
  "stale": [{"series": "flows.small", "last_obs": "2026-09-30", "expected_by": "2026-10-14"}],
  "outliers": [{"series": "pe.smallcap250", "obs_date": "2026-10-31", "value": 48.2, "z": 4.1}],
  "insufficient_history": [{"series": "pctile.180m.spread.small_large", "obs": 3, "min": 24}],
  "missing_scores": ["score.metal.silver.industrial"]
}
```

## Dashboard support (CORE)

The backend must serve every screen of CORE (core.arviawealth.com) through `/v1`; the frontend will move to these routes. This adds a shared draft with saved and published versions, a synchronous evaluate path, explain, signal analytics, backtests, notes and search on top of the core plan.

**Decisions (1 Nov 2026)**

| Topic | Decision |
| --- | --- |
| API | Frontend adopts the new `/v1`; no compatibility layer for current routes |
| Migration | Not a priority: move history, versions, backtest runs and notes from the current backend later (workstream T8, starts Phase 2) |
| Connectors | Existing fetchers will change; build the connector interface and a few jobs only, no porting |
| Draft | One shared draft per model for the whole team |
| Revert | Behaviour to be confirmed; both modes built behind a flag (below) |
| Approval | Save review → Publish; no second-person approval step for now |
| Allocations | One model allocation now, modeled as a `portfolio` so per-profile or per-client allocations can be added later |
| Auth | Email + password login, session cookie; everyone is admin for now, roles kept in the schema |
| Keys | Adopt CORE's keys: nodes `equity.large`, `debt.liquid`, `metals.gold`, `sector.banking`, `l1.equity`; scores `<node>::<signal>` (e.g. `equity.large::relvalue`). Replaces the `score.*` naming used earlier in this doc |

**Screen → backend map**

| Screen | Endpoints | Backed by |
| --- | --- | --- |
| Overview | `GET /v1/portfolios/core/allocation` | Latest published version; compare to neutral or previous published |
| Signals | `GET /v1/signals`, `/signals/attribution`, `/signals/sensitivity`; `POST /v1/signals/fragility` | Engine `Evaluate` on the draft |
| Data Points | `GET /v1/registry`, `GET /v1/series/{key}/explain`, `POST /v1/sources/{key}/refresh` | Catalog metadata + datapoints + explain steps |
| Overrides | `GET/PATCH /v1/draft`, `POST /v1/draft/evaluate`, `GET/POST /v1/rubrics/{key}` | Shared draft; preview never writes |
| New Run, Run History | `GET /v1/funds/search`, `GET /v1/benchmarks`, `POST/GET /v1/backtests` | `backtest.run` job |
| Compare & Publish | `GET /v1/versions/compare`, `POST /v1/versions`, `POST /v1/versions/{id}/publish` | Versions |
| History | `GET /v1/versions`, `POST /v1/versions/{id}/revert`, `DELETE /v1/versions/{id}` | Versions |
| Cadence | `GET /v1/cadence` | Review rhythms in config + latest version dates |
| Notes | `/v1/notes` CRUD | Notes + references |
| Search (⌘K) | `GET /v1/search` | Series, signals, parameters, notes |
| Methodology | `GET /v1/models/maaf/configs/active` | Active config |

### Draft and versions

- **Draft**: the model's working state. It reads the latest datapoints (auto-fetched scores flow in continuously, which is why refreshing data is not publishing), plus manual inputs, rubric answers, score overrides and a draft config. One row per model, edited with optimistic concurrency (`rev`).
- **Save review**: freezes the draft into an immutable version of kind `review`. Overview does not change.
- **Publish**: freezes the draft (or a saved review) as kind `published`, marks the previous published version `superseded`, and Overview switches to it.
- **Version snapshot**: config, every input value with its datapoint id and source, every run value, and the allocation. A version can always be reopened exactly as held.
- **Compare**: any two of draft, a version, neutral; returns changed inputs (scores, vetoes, parameters) and allocation moves.
- **Revert** (mode flag, default `manual_and_params` until confirmed): `manual_and_params` restores that version's config, rubric answers and overrides into the draft while auto-fetched scores stay live; `all` also pins every auto-fetched score to the version's value as an override with expiry.
- **Delete**: soft delete; the active published version cannot be deleted.

### Schema additions

```sql
-- portfolios: one row ('core') now; per-profile/per-client later
CREATE TABLE model.portfolios (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  key text UNIQUE NOT NULL, name text NOT NULL,
  model_id bigint NOT NULL REFERENCES model.models(id),
  risk_profile_id bigint NOT NULL REFERENCES model.risk_profiles(id),
  meta jsonb NOT NULL DEFAULT '{}'
);

CREATE TABLE model.drafts (
  model_id      bigint PRIMARY KEY REFERENCES model.models(id),
  config        jsonb NOT NULL,              -- draft parameters (weights, caps, normalisation…)
  base_version_id bigint,                    -- version the draft started from
  rev           int NOT NULL DEFAULT 1,      -- optimistic concurrency
  updated_by    bigint, updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE model.versions (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid          uuid UNIQUE NOT NULL DEFAULT gen_random_uuid(),
  model_id      bigint NOT NULL REFERENCES model.models(id),
  portfolio_id  bigint NOT NULL REFERENCES model.portfolios(id),
  kind          text NOT NULL CHECK (kind IN ('review','published')),
  status        text NOT NULL CHECK (status IN ('active','superseded','saved','deleted')),
  label         text, reason text,
  config        jsonb NOT NULL,
  inputs        jsonb NOT NULL,              -- {"equity.large::valuation": {"value":99.1,"datapoint_id":41877,"source":"computed"}, …}
  run_id        bigint REFERENCES model.model_runs(id),   -- run values + allocation
  as_of         date NOT NULL,
  created_by    bigint, created_at timestamptz NOT NULL DEFAULT now(),
  published_at  timestamptz
);
CREATE UNIQUE INDEX one_active_published ON model.versions (portfolio_id) WHERE kind = 'published' AND status = 'active';

-- registry metadata on series (Data Points screen)
ALTER TABLE catalog.series
  ADD COLUMN display_name text,
  ADD COLUMN node_key     text,               -- 'equity.large'
  ADD COLUMN signal_key   text,               -- 'relvalue'
  ADD COLUMN registry_group text,             -- equity | debt | metals | governance | veto
  ADD COLUMN update_type  text,               -- derived | rubric | analyst | constant | governance | veto | raw
  ADD COLUMN owner        text,               -- System | Model Admin | Investment Team
  ADD COLUMN stale_after  interval,           -- freshness window (Daily → 2 days, Monthly → 40 days)
  ADD COLUMN flags        text[] NOT NULL DEFAULT '{}';   -- mock | constant | default

-- market data for backtests: high volume, not bitemporal
CREATE TABLE market.funds (
  scheme_code text PRIMARY KEY, name text NOT NULL, amc text, category text,
  plan text, active boolean NOT NULL DEFAULT true
);
CREATE TABLE market.fund_navs (scheme_code text NOT NULL, nav_date date NOT NULL, nav numeric(14,4) NOT NULL,
  PRIMARY KEY (scheme_code, nav_date));
CREATE TABLE market.index_levels (index_key text NOT NULL, level_date date NOT NULL, value numeric(14,4) NOT NULL,
  PRIMARY KEY (index_key, level_date));     -- nifty50, nifty100, midcap150, smallcap250, niftybank

CREATE TABLE analytics.backtests (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid          uuid UNIQUE NOT NULL DEFAULT gen_random_uuid(),
  name          text,
  status        text NOT NULL CHECK (status IN ('queued','running','completed','failed','cancelled')),
  source        jsonb NOT NULL,             -- {"kind":"draft"} | {"kind":"version","id":"…"}
  allocation    jsonb NOT NULL,             -- frozen sleeve weights at submit time
  benchmark     text NOT NULL,
  legs          jsonb NOT NULL,             -- [{"sleeve":"equity.large","scheme_code":"120586","amount":25400,"purchase_date":"2021-11-01"}]
  summary       jsonb,                      -- invested, current, abs return, XIRR, diffs
  series_uri    text,                       -- daily value series (blob)
  per_fund      jsonb,
  warnings      jsonb NOT NULL DEFAULT '[]',   -- e.g. missing pre-purchase NAV history
  job_run_id    bigint, rerun_of bigint,
  created_by    bigint, created_at timestamptz NOT NULL DEFAULT now(), finished_at timestamptz
);

CREATE TABLE app.notes (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, uuid uuid UNIQUE NOT NULL DEFAULT gen_random_uuid(),
  title text NOT NULL, body_md text NOT NULL,
  author_id bigint NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE app.note_refs (note_id bigint NOT NULL REFERENCES app.notes(id), ref_kind text NOT NULL, ref_key text NOT NULL,
  PRIMARY KEY (note_id, ref_kind, ref_key));   -- series | node | parameter | version | backtest

ALTER TABLE auth.users ADD COLUMN password_hash text;   -- argon2id
CREATE TABLE auth.sessions (id text PRIMARY KEY, user_id bigint NOT NULL REFERENCES auth.users(id),
  created_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL, revoked_at timestamptz);
```

### Engine additions

- `Evaluate(state, edits)` is a pure function: no writes, target < 50 ms. `model.run`, versions, preview, sensitivity and fragility all call it, so every number on every screen comes from one engine.
- Normalisation modes: `proportional` (default) and `zero_sum`, chosen in config.
- Governance parameters read by primitives: `percentileWindowYears`, `percentileMinObservations`, `respectDefinitionBreaks` (percentiles skip history before a series' definition break date, stored in `series.meta.definition_breaks`).
- Explain: every primitive implements `Explain(...) []Step`, so the formula breakdown is generated, not hand-written.
- Signal contribution = (score − 50) × weight, in composite points.
- Change attribution: draft vs last saved review per line item; lines under 0.1pp are hidden.
- Sensitivity: each score ±10 points, re-evaluated; impact = max absolute move of any line in pp; cached per draft `rev`.
- Fragility: every signal weight ±10% (renormalized), re-evaluated; reports how far each rollup moves. Synchronous, about 200 evaluations.
- Neutral portfolio weights computed for compare = neutral.

### Jobs: `backtest.run` is the sixth job

| Field | Value |
| --- | --- |
| Queue | `backtest`, concurrency 1 |
| Limit | Up to 3 queued or running; a 4th submit returns `409 queue_full` |
| Args | `{backtest_id}` |
| Steps | Load frozen allocation and legs → load NAVs and benchmark levels point-in-time (latest on or before each date) → invest each leg and the same cash flow into the benchmark → daily value series → absolute return, capital-aware return line, money-weighted XIRR, per-fund results → store summary, series blob, warnings |
| Output | `{invested, current, abs_return, xirr, vs_benchmark}` |
| Re-run | New backtest row with `rerun_of`, same inputs, fresh data |

Fund NAVs and index levels come from the existing fetchers until connectors are rebuilt; `import.file` mappings for `market.fund_navs` and `market.index_levels` are the fallback.

### API additions

`POST /v1/auth/login`

```json
{"email": "shubham@arviawealth.com", "password": "…"}
```

`200 OK` sets an HttpOnly session cookie and returns `{"user": {"email": "shubham@arviawealth.com", "role": "admin"}}`. Also `POST /v1/auth/logout`, `GET /v1/me`.

`GET /v1/portfolios/core/allocation?version=published&compare=neutral`

```json
{
  "portfolio": "core",
  "version": {"id": "7c1e…", "kind": "published", "as_of": "2026-10-28", "published_at": "2026-10-28T05:26:00Z"},
  "asset_classes": [
    {"node": "l1.equity", "weight": 0.5727, "compare": 0.55, "composite": 54.9},
    {"node": "l1.debt", "weight": 0.3355, "compare": 0.35, "composite": 34.7},
    {"node": "l1.metals", "weight": 0.0918, "compare": 0.10, "composite": 24.8}
  ],
  "sleeves": [
    {"node": "equity.large", "group": "l1.equity", "weight": 0.254, "within_group": 0.480, "neutral": 0.224, "delta": 0.0303, "veto": false},
    {"node": "equity.small", "group": "l1.equity", "weight": 0.073, "within_group": 0.139, "neutral": 0.085, "delta": -0.0113, "veto": true},
    {"node": "sector.banking", "group": "l1.equity", "weight": 0.043, "within_group": 0.075, "neutral": 0.0, "sleeve": true}
  ],
  "drivers": {"l1.equity": {"valuation": 84.9, "macro": 25.0, "fundamentals": 55.0, "flows": 73.8, "momentum": 15.8, "composite": 54.9}},
  "vetoes": [{"node": "equity.small", "source": "override", "note": "Manually flagged"}]
}
```

`GET /v1/draft`

```json
{
  "model": "maaf",
  "rev": 42,
  "base_version": "7c1e…",
  "changed_since_published": {"scores": 15, "vetoes": 0, "parameters": 0},
  "config": {"signal_weights": {"l1": {"valuation": 0.30, "…": "…"}}, "max_tilt": {"l1": 0.20, "equity": 0.30, "debt": 0.30, "metals": 0.25}, "normalisation": "proportional"},
  "overrides": [{"key": "equity.large::margin_cycle", "value": 50, "reason": "No feed", "expires": "2027-01-31"}]
}
```

`POST /v1/draft/evaluate` — preview only, writes nothing

```json
{
  "rev": 42,
  "edits": [
    {"op": "set_param", "path": "/max_tilt/l1", "value": 0.15},
    {"op": "set_score", "key": "sector.capgoods::revisions", "value": 70},
    {"op": "set_rubric", "key": "l1.equity::macro", "conditions": {"rbi_stance": "cutting", "growth": "in_line", "inflation": "falling"}}
  ]
}
```

```json
{
  "allocation": {"l1.equity": 0.5690, "l1.debt": 0.3390, "l1.metals": 0.0920},
  "sleeves": {"equity.large": 0.2531, "sector.capgoods": 0.0000, "…": "…"},
  "diff_vs_saved": [{"node": "equity.large", "from": 0.2545, "to": 0.2531, "delta": -0.0014}],
  "warnings": []
}
```

`PATCH /v1/draft` takes the same `edits` plus `reason`, applies them (scores and rubric answers become datapoints with source `override` or `analyst`), and returns the new `rev`; a stale `rev` returns `409 conflict` with the current draft. `POST /v1/draft/reset` drops unsaved edits.

`GET /v1/rubrics/l1.equity::macro`

```json
{
  "key": "l1.equity::macro",
  "base": 50,
  "conditions": [
    {"id": "rbi_stance", "label": "RBI stance", "options": [{"id": "cutting", "label": "Cutting / about to cut", "add": 20}, {"id": "hold", "label": "On hold", "add": 0}, {"id": "hiking", "label": "Hiking / hawkish", "add": -20}]},
    {"id": "growth", "label": "Growth vs. expectations", "options": [{"id": "beat", "add": 10}, {"id": "in_line", "add": 0}, {"id": "miss", "add": -10}]},
    {"id": "inflation", "label": "Inflation direction", "options": [{"id": "falling", "add": 5}, {"id": "rising", "add": -5}]}
  ],
  "current": {"score": 25.0, "answered_at": "2026-08-01", "stale_days": 92}
}
```

`GET /v1/registry?group=equity&source=all&group_by=asset_class`

```json
{
  "totals": {"tracked": 110, "live": 104, "mock": 6, "by_group": {"equity": 57, "debt": 18, "metals": 11, "governance": 15, "veto": 9}},
  "rows": [
    {"key": "equity.large::valuation", "name": "Large Cap - Valuation", "group": "equity", "update_type": "derived", "source": "Model pipeline", "owner": "System",
     "value": 99.1, "freshness": "stale", "frequency": "daily", "last_updated": "2026-11-01", "observed_at": "2026-07-31", "flags": [], "overridden": false,
     "upstream": [{"key": "nifty100_pe", "value": 22.1}]}
  ]
}
```

`GET /v1/series/l1.equity::valuation/explain`

```json
{
  "key": "l1.equity::valuation",
  "score": 84.9,
  "sources": ["NSE", "FRED"],
  "observed": "2026-10-09",
  "freshness": "stale",
  "formula": "score = (a + b) / 2",
  "inputs": [
    {"key": "nifty50_pe", "label": "Nifty 50 trailing P/E", "value": 19.27, "source": "NSE", "observed": "2026-10-09"},
    {"key": "gsec_10y", "label": "10Y G-sec yield", "value": 6.78, "unit": "%", "source": "FRED", "observed": "2026-07-01"}
  ],
  "parts": [
    {"id": "a", "title": "Earnings-yield gap", "result": 70.3, "direction": "as_is",
     "steps": ["Earnings yield = 100 ÷ 20.78 = 4.81%", "Gap = 4.81% − 6.78% = −1.97%", "PERCENTRANK(708 readings over 10.7y, −1.97%) × 100 = 70.3"]},
    {"id": "b", "title": "P/E percentile", "result": 99.5, "direction": "inverted",
     "steps": ["PERCENTRANK(755 readings over 10.9y, 19.27) × 100 = 0.53", "100 − 0.53 = 99.5"]}
  ],
  "final": "(70.30 + 99.47) / 2 = 84.9",
  "notes": ["Part a month-joins a daily P/E to a monthly yield; its newest point is the last month both exist."]
}
```

`GET /v1/signals/sensitivity?limit=10`

```json
{"draft_rev": 42, "bump": 10, "rows": [{"key": "sector.capgoods::valuation", "impact_pp": 8.59}, {"key": "l1.equity::valuation", "impact_pp": 0.57}]}
```

`POST /v1/signals/fragility` returns `{"perturbation": 0.10, "rollups": [{"node": "l1.equity", "min": 0.561, "max": 0.583, "range_pp": 2.2}]}`.

`POST /v1/versions` — save review or publish the draft

```json
{"kind": "published", "label": "Q4 review", "reason": "Quarterly sector review; capgoods enters sleeve"}
```

```json
{"id": "9a40…", "kind": "published", "status": "active", "frozen_inputs": 15, "superseded": "7c1e…"}
```

`GET /v1/versions/compare?a=draft&b=published`

```json
{
  "totals": {"changes": 15, "scores": 15, "vetoes": 0, "parameters": 0},
  "allocation_moves": [{"node": "equity.large", "from": 0.2544, "to": 0.2545, "delta": 0.0001}],
  "inputs": [{"key": "equity.large::relvalue", "kind": "score", "source": "auto", "from": 59.7, "to": 59.9}]
}
```

`POST /v1/versions/{id}/revert` with `{"mode": "manual_and_params"}` or `{"mode": "all"}` returns the new draft `rev` and the compare result. `POST /v1/versions/{id}/publish` publishes a saved review. `DELETE /v1/versions/{id}` soft-deletes (refused for the active published version).

`POST /v1/backtests`

```json
{
  "name": "Current allocation, 5y",
  "source": {"kind": "draft"},
  "benchmark": "midcap150",
  "total_amount": 100000,
  "purchase_date": "2021-11-01",
  "legs": [
    {"sleeve": "equity.large", "scheme_code": "120586", "amount": 25440},
    {"sleeve": "debt.liquid", "scheme_code": "119800", "amount": 13180}
  ]
}
```

`202 Accepted` returns `{"backtest": {"id": "245b…", "status": "queued", "position": 1}, "job_run": {…}}`. `GET /v1/backtests/{id}` when completed:

```json
{
  "id": "245b…", "status": "completed", "benchmark": "midcap150",
  "summary": {
    "current": {"allocation": 166136, "benchmark": 198483, "difference": -32347},
    "abs_return": {"allocation": 0.6613, "benchmark": 0.9848, "difference": -0.3235},
    "xirr": {"allocation": 0.1116, "benchmark": 0.1519, "difference": -0.0403}
  },
  "series_url": "/v1/backtests/245b…/series?range=ALL",
  "per_fund": [{"sleeve": "equity.large", "scheme_code": "120586", "invested": 25440, "current": 44120, "xirr": 0.118}],
  "warnings": []
}
```

Also: `GET /v1/funds/search?q=icici` (min 3 characters), `GET /v1/benchmarks`, `POST /v1/backtests/{id}/rerun`, `GET /v1/cadence` (each rhythm with `every_days`, `last_entry`, `days_remaining`), `GET /v1/search?q=valuation` (series, nodes, parameters, notes), `/v1/notes` CRUD where `[[equity.large::valuation]]` in `body_md` becomes a reference, and `POST /v1/sources/{key}/refresh` (enqueues the source's fetch job, or `409 not_available` while its connector is not built).

### Migration from the current backend (T8)

1. Inventory the current store: versions, overrides and rubric answers, raw series history, backtest runs, notes, users.
2. Map each to the new schema: versions → `model.versions`; overrides and rubric answers → datapoints (`override` / `analyst`); raw history → datapoints (`file_import`, `known_at` from publication lag); backtests → `analytics.backtests`; notes → `app.notes` with references re-parsed.
3. Build an idempotent `app migrate-legacy` command that can be re-run until cut-over.
4. Reconcile: the latest published version's allocation must reproduce within 0.05pp, and counts must match per entity.
5. Cut over: freeze writes on the old backend, final migrate, point the frontend at `/v1`.

### Added tasks

| Id | Phase | Team | Task | Acceptance |
| --- | --- | --- | --- | --- |
| P1-T6-06 | P1 | T6 | Email + password auth, sessions, `GET /v1/me` | Login, logout, expiry tests |
| P1-T1-07 | P1 | T1 | Registry metadata columns + `GET /v1/registry` | Matches the Data Points screen fields |
| P2-T3-07 | P2 | T3 | `Explain()` on every primitive + explain endpoint | Reproduces the Equity - Valuation breakdown |
| P2-T8-01 | P2 | T8 | Inventory current backend + mapping doc | Every entity mapped or explicitly dropped |
| P3-T5-07 | P3 | T5 | `Evaluate` pure path, normalisation modes, neutral portfolio weights | Preview p95 < 50 ms |
| P3-T5-08 | P3 | T5 | Shared draft, versions, compare, revert (both modes), publish | Publish → Overview switches; stale rev → 409 |
| P3-T5-09 | P3 | T5 | Attribution, sensitivity, fragility | Matches CORE's current numbers on the same inputs |
| P3-T6-07 | P3 | T6 | Draft, rubric, versions, signals, cadence, search, notes endpoints | Conformance tests |
| P3-T8-02 | P3 | T8 | `app migrate-legacy` for raw history, overrides, notes | Re-run is a no-op |
| P3-T9-01 | P3 | T9 | Funds, NAVs, index levels tables + import mappings | 5 benchmarks and fund search work |
| P3-T9-02 | P3 | T9 | `backtest.run` job + backtest endpoints | Reproduces an existing CORE report within 0.1% XIRR |
| P4-T8-03 | P4 | T8 | Migrate versions + backtests, reconcile, cut over | Published allocation matches within 0.05pp |

New teams: **T8 Migration** (owns `cmd/migrate-legacy/`) and **T9 Backtest** (owns `analytics/`, `market/`).

## Execution plan with agent teams

A lead agent freezes contracts in Phase 0; then six teams build in parallel across five phases (Phase 5 adds connectors per source, once confirmed), each phase closing on a testable gate. Teams own directories, so parallel work never edits the same files.

&#91;embedded content: build roadmap · 5 phases, 5 gates\]

G3 is the critical gate: the engine must reproduce every workbook output before hardening starts.

**Teams**

| Team | Owns | Mandate |
| --- | --- | --- |
| Lead (architect) | `internal/*/contract.go`, `migrations/`, `api/openapi.yaml`, `configs/seed/` schemas | Freezes interfaces and schema, merges PRs, resolves contract changes, runs gates |
| T1 Data | `catalog/`, `datapoints/`, `platform/` | Catalog, writer, PIT reader, outbox, raw payloads |
| T2 Jobs | `jobs/` | Registry, middleware, River adapter, scheduler, outbox dispatcher, job kinds wiring |
| T3 Derive | `derive/` | Primitives, cascade planner, rebuild |
| T4 Ingest | `ingest/` | File import, import mappings, connector interface; source connectors in Phase 5 |
| T5 Model | `model/` | Config loader and validator, steps, constraints, runs, allocations |
| T6 API | `api/`, `auth/`, `audit/` | Handlers, auth, roles, audit, SSE, OpenAPI conformance |
| T7 Verify | `testdata/`, `e2e/` | Sheet fixtures, golden outputs, e2e tests; independent reviewer of every gate |

**Working rules for agents**

1. One task = one git worktree + one PR; PR title starts with the task id (e.g. `P1-T1-03`).
2. Code against interfaces in `contract.go`; use generated mocks (`mockery`) for other teams' modules.
3. Contract changes go through the lead as a separate PR; affected teams are notified in `HANDOFF.md`.
4. Definition of done: `make lint test` green, ≥ 80% coverage on new packages, sqlc regenerated, OpenAPI updated if an endpoint changed, `HANDOFF.md` entry with what's done and what's next.
5. T7 reviews each phase gate without having written the code under review.
6. No team edits another team's directory; shared fixes are filed as issues to the owner.

### Phase 0 — Contracts (lead only)

| Id | Task | Output | Acceptance |
| --- | --- | --- | --- |
| P0-01 | Repo scaffold, `cmd/app` modes, Makefile, docker-compose (Postgres 16, MinIO), CI (lint, test, sqlc diff) | Running skeleton | `make up && make test` green |
| P0-02 | All migrations from the schema section | `migrations/0001…` | Up and down clean on empty DB |
| P0-03 | `contract.go` per module: every interface in this doc | Compiling stubs | `go build ./...` passes |
| P0-04 | OpenAPI 3.1 spec for every endpoint in the API section | `api/openapi.yaml` | Lints clean; examples match this doc |
| P0-05 | Seed YAML schemas: entities, series, derivations, model config, schedules, profiles | JSON Schemas + example files | Validator CLI accepts examples |
| P0-06 | `HANDOFF.md`, team ownership in `CODEOWNERS` | Docs | Reviewed |

Gate G0: contracts frozen; teams start.

### Phase 1 — Foundations (T1, T2, T6, T7 in parallel)

| Id | Team | Task | Acceptance |
| --- | --- | --- | --- |
| P1-T1-01 | T1 | Catalog repo: entities, sources, series CRUD, validation rules | Unit tests per rule |
| P1-T1-02 | T1 | `series_deps` builder + cycle detection on save | Cycle returns `ErrCycle` |
| P1-T1-03 | T1 | `Writer.Write`: validate, no-op skip, insert, outbox in one tx | Tx rollback leaves no rows or outbox |
| P1-T1-04 | T1 | `Reader.At/Range/History/Versions` with source priority + retractions | PIT table tests incl. revisions and overrides |
| P1-T1-05 | T1 | Raw payload store (MinIO) | Round-trip with sha256 |
| P1-T2-01 | T2 | Registry, `Spec`, generic `Register`, args JSON Schema generation | Duplicate kind panics at boot |
| P1-T2-02 | T2 | River adapter: `Enqueue`, `EnqueueTx`, unique, snooze, retry mapping | Adapter contract tests |
| P1-T2-03 | T2 | Middleware chain + `job_runs` bookkeeping + progress + cancel | Cancel stops a sleeping job within 3s |
| P1-T2-04 | T2 | `scheduler.tick` with cron, interval; overlap + catch-up policies | Fake clock tests: no double fire, catch-up modes |
| P1-T2-05 | T2 | `outbox.dispatch` + `Subscriber` registry | Exactly-once enqueue per outbox row |
| P1-T6-01 | T6 | chi router, API keys, roles, error shape, request ids, audit middleware | 401/403 tests |
| P1-T6-02 | T6 | Catalog and datapoints endpoints | OpenAPI conformance tests |
| P1-T7-01 | T7 | Convert the workbook into fixtures: 4 tracker logs, all 85 scores, all expected outputs | `testdata/sheet_v2/*.json` |
| P1-T7-02 | T7 | Golden expectations list (every composite, tilt, final, rollup weight) | Reviewed against sheet by hand |

Gate G1: `POST /v1/datapoints` → rows + outbox → a stub subscriber job runs → visible in `GET /v1/jobs/{id}/tree`.

### Phase 2 — Compute and ingest (T3, T4, T2, T6, T7)

| Id | Team | Task | Acceptance |
| --- | --- | --- | --- |
| P2-T3-01 | T3 | Primitives: expr, rolling\_mean, ratio\_of, pct\_change, diff\_over | Table tests |
| P2-T3-02 | T3 | `percentile_rank` point-in-time with `min_obs` | Never reads obs\_date > d; null below min |
| P2-T3-03 | T3 | invert, avg, rubric, bucket\_map, const, fallback, consecutive, threshold | Rubric examples from Calculation Guide pass |
| P2-T3-04 | T3 | **Job 2** `derive.cascade` incremental: planner (topo sort, ranges) + executor + input\_hash skip | Re-run writes 0 rows |
| P2-T3-05 | T3 | **Job 2** `derive.cascade` rebuild mode | Rebuild after version bump keeps old rows |
| P2-T4-01 | T4 | Uploads, import mappings, parser for CSV and XLSX | Parses all 4 tracker logs |
| P2-T4-02 | T4 | **Job 1** `import.file` with dry run and per-row rejections | Imports the 4 sheet logs; dry run writes nothing |
| P2-T4-03 | T4 | `Connector` interface + registry only (no connectors) | Fake connector passes contract test |
| P2-T3-06 | T3 | **Job 5** `quality.check`: stale, missing scores, insufficient history, outliers, bad categories | Report matches fixtures |
| P2-T2-06 | T2 | Calendar schedule types: business\_day, month\_day, month\_end+Nbd, review | NSE holiday calendar tests |
| P2-T6-03 | T6 | Jobs, schedules, uploads, import-mappings, quality endpoints + SSE | Conformance + SSE test |
| P2-T7-03 | T7 | Seed series YAML for every workbook series and derivation | Loads with no validation errors |

Gate G2: import the sheet's logs → every percentile and derived series computes → no value uses future data (T7 look-ahead test) → re-import is a no-op.

### Phase 3 — Model (T5, T6, T7)

| Id | Team | Task | Acceptance |
| --- | --- | --- | --- |
| P3-T5-01 | T5 | Config loader + validator (weights sum, refs exist, step names registered) | Bad configs return pointer errors |
| P3-T5-02 | T5 | Steps: composite, veto, tilt, normalize | Matches sheet L1 and L2 |
| P3-T5-03 | T5 | Sector qualify + sleeve carve-out + rollup | Matches Portfolio Rollup |
| P3-T5-04 | T5 | Constraints: max\_multiple\_of\_neutral, extreme\_full\_cap, sector\_max\_tenure | Unit tests per rule |
| P3-T5-05 | T5 | **Job 4** `model.run`: run\_values with inputs, node overrides, lifecycle, `publish` flag | Run is reproducible from stored inputs |
| P3-T5-06 | T5 | **Job 3** `veto.evaluate` | Small-cap veto fires from trigger datapoints |
| P3-T6-04 | T6 | Configs, runs, allocations, overrides, risk profiles endpoints | Conformance tests |
| P3-T7-04 | T7 | Golden test: sheet scores in → every sheet output out | All within 0.05 percentage points |

Gate G3: the golden test passes, and a what-if run with a draft config returns a different allocation without touching published state.

### Phase 4 — Hardening (all teams)

| Id | Team | Task | Acceptance |
| --- | --- | --- | --- |
| P4-T1-06 | T1 | Partition-ready indexes, query plans under 10M rows | PIT range p95 < 20 ms |
| P4-T6-05 | T6 | Rate limits, OpenTelemetry, `/metrics`, `/readyz` | Dashboards show per-kind job metrics |
| P4-T7-05 | T7 | E2E: upload tracker → cascade → veto → quality → run → approve → publish | Green in CI with fake clock |
| P4-Lead | Lead | Staging deploy, seed schedules enabled, runbook for the monthly upload | 7 consecutive days of scheduled jobs green |

Gate G4: MVP done.

### Phase 5 — Connectors (per source, once confirmed)

Each source is an independent task: one connector, its recorded-response tests and a schedule row. Nothing outside `ingest/` changes, and the monthly file upload stays as a fallback.

| Id | Team | Task | Acceptance |
| --- | --- | --- | --- |
| P5-T4-01 | T4 | `ingest.fetch` + `ingest.backfill` kinds on the Phase 2 connector interface | Fake connector e2e |
| P5-T4-02 | T4 | FRED (`DFII10`) | Recorded-response tests; schedule `fred-daily` |
| P5-T4-03 | T4 | AMFI category flows and AUM | Same; schedule `amfi-monthly` |
| P5-T4-04 | T4 | RBI/CCIL yields, repo; FIMMDA AAA | Same |
| P5-T4-05 | T4 | NSE index P/E and sector indices | Same |
| P5-T4-06 | T4 | Bullion prices | Same |

Gate G5 (per source): a scheduled fetch replaces that source's columns in the monthly upload, and `quality.check` stops flagging them.

**Parallelism by phase**

| Phase | Teams active | Blocking dependency |
| --- | --- | --- |
| P0 | Lead | — |
| P1 | T1, T2, T6, T7 | G0 |
| P2 | T2, T3, T4, T6, T7 | T1 writer/reader (P1-T1-03/04) |
| P3 | T5, T6, T7 | Seeded score datapoints (G2) |
| P4 | T1, T6, T7, Lead | G3 |
| P5 | T4 (+ T2 for schedules) | A confirmed data source; can start per source any time after G2 |

**Prompt template for a team agent**

```
You are team <T#> on the allocation backend. Read: docs/plan.md (this doc), HANDOFF.md,
internal/<module>/contract.go. Your task: <task id> — <task>. Only edit: <owned dirs>.
Use mocks for other modules. Done means: acceptance "<acceptance>", make lint test green,
HANDOFF.md entry. If a contract must change, stop and write a proposal in HANDOFF.md.
```

## Testing, sheet issues and open questions

Correctness is proven by a golden test against the workbook plus a look-ahead test; eight issues in the current sheet are fixed by design, and five questions need an owner's answer before Phase 3.

**Test layers**

| Layer | What | Where |
| --- | --- | --- |
| Unit | Primitives, steps, constraints, schedule calendars, validation | Each package |
| Repository | sqlc queries against real Postgres (testcontainers) | `*_db_test.go` |
| Contract | Job engine adapter, connector recorded responses, OpenAPI conformance | `jobs/river`, `ingest/*`, `api` |
| Golden | Sheet inputs → every sheet output, within 0.05pp | `e2e/golden_test.go` |
| Look-ahead | For every computed datapoint, all inputs have `obs_date ≤` its date and `known_at ≤` its `known_at` | `e2e/pit_test.go` |
| E2E | Fake clock drives schedules through to a published allocation | `e2e/pipeline_test.go` |

**Sheet issues fixed by design**

| Issue in the workbook | Fix |
| --- | --- |
| Percentiles rank each month against future months (Jan-2026 10Y %ile = 100 because of Feb–Mar) | `percentile_rank` reads history ≤ d only |
| Gold Ret %ile filled while Gold 12M Return is empty | Derived from `ret.gold.12m`; null without 12 months |
| Flow %iles rank raw ₹ crore, not 3M avg % of AUM | `flows.*.3m_pct_aum` window series feed the percentile |
| Scores are typed, not linked to trackers (Large Cap relative value 60 vs computed 50) | Scores are derived series; analyst entries are explicit overrides |
| Veto shows Y with no recorded trigger | Triggers are datapoints with lineage |
| Metals ≤ 2× neutral, 4-quarter sector exit, extreme full-cap not enforced | Constraint registry |
| 3 rows of history produce 0/50/100 percentiles | `min_history` → null → score 50 with warning |
| No record of who changed what | Append-only datapoints + audit log |

**Open questions**

- [ ] Revert: confirm on CORE whether reverting copies auto-fetched scores (mode `all`) or only manual inputs and parameters (`manual_and_params`, current default).
- [ ] Migration: which database and language does the current backend use, and how do we get read access?
- [ ] Fund NAV and index history: which existing fetcher feeds them, and how far back must backtests go?
- [ ] Extreme-score rule: what normal cap does "allow full-cap tilt" relax, given tilts are linear?
- [ ] Percentile windows per series: default 10.7–10.9 years in CORE today; confirm `percentileWindowYears` per series group.
- [ ] Revision breadth: manual monthly entry, or a paid feed (Bloomberg, Refinitiv, ACE Equity)?
- [ ] Publication lags for `known_at` on historical imports, per source.
