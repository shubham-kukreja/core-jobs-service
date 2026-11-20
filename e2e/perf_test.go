package e2e

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/civil"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/derive"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/testdb"
)

// TestPerfPointInTime is P4-T1-06: point-in-time reads stay fast at 10M
// datapoints. It is opt-in (PERF_ROWS=10000000 make perf) because loading
// takes minutes and a few GB of disk.
//
// Shape: daily series (one row per day, 5% revised later, a few
// retractions) plus monthly computed series with three versions each and
// lineage arrays, the way cascades write them. Acceptance: Range p95 < 20 ms.
func TestPerfPointInTime(t *testing.T) {
	rows, _ := strconv.Atoi(os.Getenv("PERF_ROWS"))
	if rows <= 0 {
		t.Skip("set PERF_ROWS (e.g. 10000000) to run the point-in-time load test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	db := testdb.New(t)

	const days = 2400 // ~6.5 years of daily observations per series
	const monthly = 400
	daily := max(1, (rows-monthly*360*3)*100/105/days)
	start := time.Now()
	loadPerf(ctx, t, db.Owner, daily, days, monthly)
	var total int64
	if err := db.Owner.QueryRow(ctx, `SELECT count(*) FROM dp.datapoints`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	var size string
	if err := db.Owner.QueryRow(ctx, `SELECT pg_size_pretty(pg_total_relation_size('dp.datapoints'))`).Scan(&size); err != nil {
		t.Fatal(err)
	}
	t.Logf("loaded %d rows (%d daily series × %d days, %d monthly series) in %s; table + indexes %s",
		total, daily, days, monthly, time.Since(start).Round(time.Second), size)

	cat := catalog.New(db.Runtime, derive.NewDefaultRegistry())
	dps := datapoints.New(db.Runtime, cat, clock.System{})
	rng := rand.New(rand.NewPCG(1, 2))
	first := civil.Date{Year: 2020, Month: 1, Day: 1}
	pastCutoff := time.Date(2024, 6, 30, 0, 0, 0, 0, time.UTC)
	dailyKey := func() string { return fmt.Sprintf("perf.d%d", rng.IntN(daily)) }
	monthKey := func() string { return fmt.Sprintf("perf.m%d", rng.IntN(monthly)) }
	randDay := func() civil.Date { return first.AddDays(rng.IntN(days - 370)) }

	// Warm the catalog cache and the connection pool.
	for range 20 {
		if _, err := dps.Range(ctx, dailyKey(), first, first.AddDays(30), datapoints.ReadOpts{}); err != nil {
			t.Fatal(err)
		}
	}

	type probe struct {
		name   string
		limit  time.Duration // 0: report only
		run    func() (int, error)
		minHit int // mean rows per call must reach this, to prove the probe reads data
	}
	probes := []probe{
		{"Range 1y daily, cutoff now", 20 * time.Millisecond, func() (int, error) {
			d := randDay()
			r, err := dps.Range(ctx, dailyKey(), d, d.AddDays(365), datapoints.ReadOpts{})
			return len(r), err
		}, 300},
		{"Range 1y daily, cutoff 2024-06-30", 20 * time.Millisecond, func() (int, error) {
			d := randDay()
			r, err := dps.Range(ctx, dailyKey(), d, d.AddDays(365), datapoints.ReadOpts{Cutoff: pastCutoff})
			return len(r), err
		}, 50},
		{"Range 30y monthly computed", 20 * time.Millisecond, func() (int, error) {
			r, err := dps.Range(ctx, monthKey(), civil.Date{Year: 1996, Month: 1, Day: 1}, civil.Date{Year: 2026, Month: 12, Day: 31}, datapoints.ReadOpts{})
			return len(r), err
		}, 300},
		{"History 36m monthly", 0, func() (int, error) {
			r, err := dps.History(ctx, monthKey(), civil.Date{Year: 2025, Month: 12, Day: 31}, 36, datapoints.ReadOpts{})
			return len(r), err
		}, 30},
		{"At one day", 0, func() (int, error) {
			r, err := dps.At(ctx, dailyKey(), randDay(), datapoints.ReadOpts{})
			if r == nil {
				return 0, err
			}
			return 1, err
		}, 1},
		{"Snapshot 100 series", 0, func() (int, error) {
			var keys []string
			for _, i := range rng.Perm(daily)[:min(100, daily)] {
				keys = append(keys, fmt.Sprintf("perf.d%d", i))
			}
			r, err := dps.Snapshot(ctx, keys, randDay(), datapoints.ReadOpts{})
			return len(r), err
		}, min(100, daily) * 9 / 10},
		{"Versions one day", 0, func() (int, error) {
			r, err := dps.Versions(ctx, dailyKey(), randDay())
			return len(r), err
		}, 1},
	}
	const n = 400
	for _, p := range probes {
		lat := make([]time.Duration, n)
		got := 0
		for i := range n {
			s := time.Now()
			k, err := p.run()
			lat[i] = time.Since(s)
			if err != nil {
				t.Fatalf("%s: %v", p.name, err)
			}
			got += k
		}
		slices.Sort(lat)
		p50, p95, p99 := lat[n/2], lat[n*95/100], lat[n*99/100]
		t.Logf("%-36s p50 %6.2f ms  p95 %6.2f ms  p99 %6.2f ms  (%d rows/call)", p.name, ms(p50), ms(p95), ms(p99), got/n)
		if got/n < p.minHit {
			t.Errorf("%s returned %d rows per call; the probe is not reading data", p.name, got/n)
		}
		if p.limit > 0 && p95 > p.limit {
			t.Errorf("%s p95 %s exceeds %s", p.name, p95, p.limit)
		}
	}

	// Plans, for the record: every probe must use an index, never scan the table.
	var sid int64
	if err := db.Owner.QueryRow(ctx, `SELECT id FROM catalog.series WHERE key = 'perf.d7'`).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]string{
		"range": `EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) SELECT * FROM dp.datapoints d
 WHERE d.series_id = $1 AND d.obs_date BETWEEN '2021-01-01' AND '2021-12-31' AND d.status = 'active'
   AND d.known_at <= now() AND d.source_id = ANY('{1,2,3}'::bigint[])
   AND NOT EXISTS (SELECT 1 FROM dp.datapoints r WHERE r.supersedes_id = d.id AND r.status = 'retracted' AND r.known_at <= now())`,
		"versions": `EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) SELECT * FROM dp.datapoints d
 WHERE d.series_id = $1 AND d.obs_date = '2021-06-15' ORDER BY d.known_at DESC, d.id DESC`,
	} {
		plan := explain(ctx, t, db.Owner, q, sid)
		t.Logf("plan %s:\n%s", name, plan)
		if strings.Contains(plan, "Seq Scan on datapoints") {
			t.Errorf("%s plan scans the table", name)
		}
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func explain(ctx context.Context, t *testing.T, pool *pgxpool.Pool, q string, args ...any) string {
	t.Helper()
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}

// loadPerf bulk-loads the synthetic history server-side, a chunk of series
// per statement so no single transaction holds millions of rows.
func loadPerf(ctx context.Context, t *testing.T, pool *pgxpool.Pool, daily, days, monthly int) {
	t.Helper()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO catalog.sources(key,kind,name) VALUES
  ('override','override','Override'), ('analyst','analyst','Analyst'), ('file_import','file_import','Upload'), ('computed','computed','Computed')`)
	exec(`INSERT INTO catalog.series(key,name,layer,value_type,frequency)
  SELECT 'perf.d' || i, 'Perf daily ' || i, 'raw', 'num', 'daily' FROM generate_series(0, $1 - 1) i`, daily)
	exec(`INSERT INTO catalog.series(key,name,layer,value_type,frequency,derivation,derivation_ver)
  SELECT 'perf.m' || i, 'Perf monthly ' || i, 'score', 'num', 'monthly',
         '{"primitive":"identity","inputs":{"x":"perf.d0"}}', 1 FROM generate_series(0, $1 - 1) i`, monthly)

	// Daily rows: one per series per day, known two days after the date,
	// from the upload source; 5% are revised a month later.
	const chunk = 200
	for lo := 0; lo < daily; lo += chunk {
		hi := min(lo+chunk, daily) - 1
		exec(`
INSERT INTO dp.datapoints (series_id, obs_date, known_at, value_num, source_id, batch_id)
SELECT s.id, d.obs, d.obs + interval '2 days', 100 + (s.id % 97) + d.i * 0.01, src.id,
       ('00000000-0000-4000-8000-' || lpad(to_hex(s.id * 100 + d.i / 30), 12, '0'))::uuid
FROM catalog.series s
CROSS JOIN LATERAL (SELECT i, date '2019-06-01' + i AS obs FROM generate_series(0, $3 - 1) i) d
CROSS JOIN (SELECT id FROM catalog.sources WHERE key = 'file_import') src
WHERE s.key IN (SELECT 'perf.d' || g FROM generate_series($1::int, $2::int) g)
UNION ALL
SELECT s.id, d.obs, d.obs + interval '32 days', 101 + (s.id % 97) + d.i * 0.01, src.id,
       ('00000000-0000-4000-9000-' || lpad(to_hex(s.id * 100 + d.i / 30), 12, '0'))::uuid
FROM catalog.series s
CROSS JOIN LATERAL (SELECT i, date '2019-06-01' + i AS obs FROM generate_series(0, $3 - 1) i WHERE (i * 7 + i / 13) % 20 = 0) d
CROSS JOIN (SELECT id FROM catalog.sources WHERE key = 'file_import') src
WHERE s.key IN (SELECT 'perf.d' || g FROM generate_series($1::int, $2::int) g)`, lo, hi, days)
	}
	// A few retractions (one per 2,000 rows) superseding revisions.
	exec(`
INSERT INTO dp.datapoints (series_id, obs_date, known_at, source_id, status, supersedes_id, batch_id, note)
SELECT series_id, obs_date, known_at + interval '1 day', source_id, 'retracted', id,
       '00000000-0000-4000-a000-000000000001'::uuid, 'perf retraction'
FROM dp.datapoints WHERE id % 2000 = 0`)
	// Monthly computed rows, 30 years, three versions each, with lineage.
	exec(`
INSERT INTO dp.datapoints (series_id, obs_date, known_at, value_num, source_id, derivation_ver, input_hash, inputs, batch_id)
SELECT s.id, m.obs, m.obs + (v * interval '20 days') + interval '5 days', 50 + v, src.id, 1,
       sha256((s.id || ':' || m.i || ':' || v)::text::bytea), ARRAY[s.id * 3, s.id * 3 + 1, s.id * 3 + 2]::bigint[],
       ('00000000-0000-4000-b000-' || lpad(to_hex(m.i * 10 + v), 12, '0'))::uuid
FROM catalog.series s
CROSS JOIN LATERAL (SELECT i, (date '1996-01-01' + (i + 1) * interval '1 month' - interval '1 day')::date AS obs FROM generate_series(0, 359) i) m
CROSS JOIN generate_series(0, 2) v
CROSS JOIN (SELECT id FROM catalog.sources WHERE key = 'computed') src
WHERE s.key LIKE 'perf.m%'`)
	// Serial VACUUM: Docker's default 64 MB /dev/shm is too small for
	// parallel maintenance workers on a table this size.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, q := range []string{`SET max_parallel_maintenance_workers = 0`, `VACUUM ANALYZE dp.datapoints`} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}
