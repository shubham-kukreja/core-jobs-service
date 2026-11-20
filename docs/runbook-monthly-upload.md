# Runbook: the monthly upload

Each month the analyst uploads the Data Trackers workbook and the extras sheet.
The system then recomputes every score, checks data quality, re-evaluates
vetoes and, on review dates, proposes an allocation. This page shows what to
do, when, and what to check at each step.

Commands use the local stack (`make stack-up`, API on `http://127.0.0.1:8080`)
and an API key in `$CORE_KEY`:

```sh
docker compose -f deploy/stack/compose.yml run --rm --no-deps api \
  user create-api-key --email analyst@example.com --label monthly --ttl 720h
export CORE_KEY=...   # printed once; keep it out of shell history and files
```

## Calendar

Times are Asia/Kolkata. Business days skip weekends. NSE holidays are not
listed yet (see HANDOFF.md), so they still count as business days.

| When | What runs | Who |
| --- | --- | --- |
| Business days 1–10 | The analyst uploads last month's trackers and extras | Analyst |
| Every weekday 08:30 | `quality-daily`: `quality.check` for today | Schedule |
| Business day 12 | `data-due-monthly`: stale data and missing scores for last month end | Schedule |
| Business day 12 | `veto-monthly`: `veto.evaluate` for last month end | Schedule |
| Each review date in the model config | `review-quarterly` / `review-semiannual`: `model.run`, stored as a **draft** | Schedule |
| After a review run | Approve (analyst), then publish (admin) | People |

## 1. Upload the trackers

Fill in last month's row on each log of the Data Trackers sheet. Leave formula
columns (spreads, %iles) blank: the system computes them. Then:

```sh
# Upload the workbook; note the returned "id".
curl -sS -H "Authorization: Bearer $CORE_KEY" \
  -F "file=@MultiAsset_Allocation_Framework_v2.xlsx" http://127.0.0.1:8080/v1/uploads

# Optional dry run: validates every row and writes nothing.
curl -sS -H "Authorization: Bearer $CORE_KEY" -H 'Content-Type: application/json' \
  -d '{"kind":"import.file","args":{"upload_id":"<id>","mapping_id":"data_trackers.valuation","dry_run":true}}' \
  http://127.0.0.1:8080/v1/jobs
```

Run `import.file` once per log: `data_trackers.valuation`, `data_trackers.rates`,
`data_trackers.metals`, `data_trackers.flows`. Then upload the extras sheet and
run `upload.extras` on it.

**Check:** `GET /v1/jobs/{id}` shows `succeeded`, a `rows_written` count and an
empty `rejected` list. A rejected row names its column, date and reason, such as
`out_of_range`. Fix the cell and upload again.

Re-importing an unchanged file writes nothing and recomputes nothing, so
uploading again is always safe.

## 2. Enter the analyst values

Breadth counts and judgments (RBI stance, currency stress, AMC restrictions)
go through `POST /v1/datapoints` with source `analyst`, or through the
Data Points screen.

## 3. Let it compute

Each import starts `derive.cascade` for the series it touched. The cascade
recomputes percentiles, scores and triggers, and then `veto.evaluate`.
`GET /v1/jobs?status=running` should drain within minutes.

**Check:**
- `GET /v1/quality/report?as_of=<last month end>` shows no `missing_scores`.
- Any stale series it lists are expected. A source that publishes late is
  normal. Otherwise, chase the data.
- To see why a score moved: `GET /v1/series/{key}/explain`.

## 4. Review run (on review dates)

On each review date the schedule stores a **draft** run. Nothing reaches
clients until it is published.

```sh
curl -sS -H "Authorization: Bearer $CORE_KEY" "http://127.0.0.1:8080/v1/runs?model=maaf&status=draft"
curl -sS -H "Authorization: Bearer $CORE_KEY" "http://127.0.0.1:8080/v1/runs/<run>/allocations"
curl -sS -H "Authorization: Bearer $CORE_KEY" "http://127.0.0.1:8080/v1/runs/compare?a=<current>&b=<run>"
```

Read the run's `warnings`. For example, `missing score … used 50` means an
input was older than three months at the review date.

- **Approve** (analyst): `POST /v1/runs/{id}/approve` with `{"note":"…"}`.
- **Publish** (admin only): `POST /v1/runs/{id}/publish`. This supersedes the
  previous published run. `GET /v1/allocations/current?model=maaf&profile=balanced`
  then serves the new allocation.

To try an idea first, create a draft config (`POST /v1/models/maaf/configs`
with a JSON patch). Then submit `model.run` with its `config_version`. The
published allocation and the active config do not change.

## Fixing mistakes

| Problem | Fix |
| --- | --- |
| Wrong value uploaded | Correct the cell and upload again. The new value supersedes the old one from now on. Past runs keep what they saw. |
| Value entered against the wrong date | `POST /v1/datapoints/{id}/retract` with a `reason`, then enter it correctly. |
| A job failed | Read its `error` in `GET /v1/jobs/{id}`. An admin can `POST /v1/jobs/{id}/retry` once the cause is fixed. |
| A scheduled run missed its slot (stack was down) | When the worker starts, the schedule fires its most recent missed slot (catch-up policy `latest`), not every missed one. `make stack-health` shows each day's runs. To redo an older slot, `POST /v1/schedules/{id}/run-now` or submit the job by hand. |
| A published allocation is wrong | Publish a corrected run. Publishing never deletes history, so `GET /v1/runs` keeps both. |

## Where to look

- **Grafana** (`http://127.0.0.1:3000`, folder CORE): the failed scheduled jobs
  tile must stay at 0. Queue depth and outbox lag return to 0 after each upload.
- **Jaeger** (`http://127.0.0.1:16686`): one trace per request and per job attempt.
- **`make stack-health`**: scheduled runs per day and kind for the last 7 days.
