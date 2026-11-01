# Workbook fixtures (sheet_v2)

Golden inputs and expected outputs for the model engine (gate G3: every
expected value reproduced within 0.05 percentage points).

Each `*.golden.json` file holds one scenario:

| Field | Meaning |
| --- | --- |
| `source` | Where the numbers come from. Only `plan_worked_example` exists today; `workbook` replaces it when the Multi-Asset Allocation Framework file is available (open question in HANDOFF.md) |
| `as_of` | Review date |
| `neutral` | Neutral weights per node (risk profile) |
| `scorecards` | Tilt cap per scorecard |
| `inputs` | Composite score per bucket (or per-signal scores once the workbook lands) and veto flags |
| `params` | Run parameters, e.g. sectors held |
| `expected` | Every number the engine must produce, keyed by run node key (`<step>.<node>`) |
| `tolerance_pp` | Allowed absolute difference in percentage points |

Weights are fractions (0.356 = 35.6%). Composite scores are on the 0–100 scale.

`e2e/golden` loads these files, checks each scenario is internally
consistent with the plan's formulas (so a bad fixture fails fast), and, from
Phase 3, runs the engine against them.
