package e2e

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// assertNoLookAhead is the plan's look-ahead test: for every computed
// datapoint, every input in its lineage has obs_date <= its obs_date and
// known_at <= its known_at. A violation means a value used data from its
// future, either later observations or facts not yet published.
func assertNoLookAhead(ctx context.Context, t *testing.T, db *pgxpool.Pool) (checked int) {
	t.Helper()
	const q = `
WITH computed AS (
  SELECT d.id, s.key, d.obs_date, d.known_at, d.inputs
  FROM dp.datapoints d
  JOIN catalog.series s ON s.id = d.series_id
  JOIN catalog.sources src ON src.id = d.source_id
  WHERE src.key = 'computed' AND d.status = 'active'
)
SELECT c.key, c.obs_date::text, c.known_at::text, i.obs_date::text, i.known_at::text, si.key
FROM computed c
CROSS JOIN LATERAL unnest(c.inputs) AS lin(id)
JOIN dp.datapoints i ON i.id = lin.id
JOIN catalog.series si ON si.id = i.series_id
WHERE i.obs_date > c.obs_date OR i.known_at > c.known_at
LIMIT 20`
	rows, err := db.Query(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, obs, known, inObs, inKnown, inKey string
		if err := rows.Scan(&key, &obs, &known, &inObs, &inKnown, &inKey); err != nil {
			t.Fatal(err)
		}
		t.Errorf("look-ahead: %s @%s (known %s) reads %s @%s (known %s)", key, obs, known, inKey, inObs, inKnown)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// Every computed row must carry lineage, or the check above is vacuous.
	var total, noLineage int
	if err := db.QueryRow(ctx, `
SELECT count(*), count(*) FILTER (WHERE d.inputs IS NULL OR cardinality(d.inputs) = 0)
FROM dp.datapoints d JOIN catalog.sources src ON src.id = d.source_id JOIN catalog.series s ON s.id = d.series_id
WHERE src.key = 'computed' AND d.status = 'active'
  AND NOT coalesce(s.derivation->>'fn' = 'const' AND s.derivation->'inputs' = '{}'::jsonb, false)`).Scan(&total, &noLineage); err != nil {
		t.Fatal(err)
	}
	if noLineage > 0 {
		t.Errorf("%d computed rows have no input lineage", noLineage)
	}
	return total
}
