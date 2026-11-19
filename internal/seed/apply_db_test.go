package seed_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/shubham-kukreja/core-jobs-service/internal/app"
	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/testdb"
	"github.com/shubham-kukreja/core-jobs-service/internal/seed"
)

func TestApplyShippedSeed(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	var uid int64
	if err := db.Owner.QueryRow(ctx, `INSERT INTO auth.users(email,name,role) VALUES ('lead@example.com','Lead','admin') RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	core, err := app.NewCore(db.Runtime, slog.New(slog.NewTextHandler(io.Discard, nil)), clock.System{})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join("..", "..", "configs", "seed")
	docs, err := seed.Validate(dir)
	if err != nil {
		t.Fatal(err)
	}
	want, err := seed.SeriesInputs(docs)
	if err != nil {
		t.Fatal(err)
	}

	rep, err := seed.Apply(ctx, core.SeedTarget(), docs, uid)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Series != len(want) || rep.Mappings != 5 || rep.Sources != 10 || rep.Entities != 25 ||
		rep.Models != 1 || rep.Profiles != 1 || rep.Schedules != 5 {
		t.Fatalf("report = %+v", rep)
	}
	// Every derived series is new, so all of them rebuild, in one job.
	derived := 0
	for _, s := range want {
		if s.Derivation != nil {
			derived++
		}
	}
	if len(rep.Rebuild) != derived || rep.Run == nil {
		t.Fatalf("rebuild %d series (run %v), want %d", len(rep.Rebuild), rep.Run, derived)
	}
	count := func(q string) int {
		var n int
		if err := db.Owner.QueryRow(ctx, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM catalog.series`); n != len(want) {
		t.Fatalf("catalog has %d series, want %d", n, len(want))
	}
	if n := count(`SELECT count(*) FROM catalog.series WHERE layer = 'score'`); n != 85 {
		t.Fatalf("catalog has %d scores, want 85", n)
	}
	if n := count(`SELECT count(*) FROM audit.audit_log WHERE entity = 'seed' AND action = 'apply'`); n != 2 {
		t.Fatalf("audit rows = %d", n)
	}
	cv, err := core.Model.Active(ctx, "maaf")
	if err != nil || cv.Version != 1 || cv.Config.SectorSatellite == nil || len(cv.Config.SectorSatellite.Vetoes["*"]) != 3 {
		t.Fatalf("active config = %+v, %v", cv, err)
	}
	pf, err := core.Model.Portfolio(ctx, "core")
	if err != nil || pf.ModelKey != "maaf" {
		t.Fatalf("portfolio = %+v, %v", pf, err)
	}
	if prof, err := core.Model.ProfileByID(ctx, pf.RiskProfileID); err != nil || prof.Key != "balanced" || !prof.NeutralWeights["l1.debt"].Equal(decimal.RequireFromString("0.35")) {
		t.Fatalf("profile = %+v, %v", prof, err)
	}
	if n := count(`SELECT count(*) FROM jobs.job_schedules WHERE kind IN ('model.run', 'veto.evaluate')`); n != 3 {
		t.Fatalf("model schedules = %d", n)
	}
	s, err := core.Catalog.Series(ctx, "l1.equity::valuation")
	if err != nil || s.Registry.SignalKey != "valuation" || s.Derivation == nil || s.Derivation.Fn != "expr" {
		t.Fatalf("l1.equity::valuation = %+v, %v", s, err)
	}

	// Applying the same seed again is a no-op: no version bumps, no rebuild.
	jobsBefore := count(`SELECT count(*) FROM jobs.job_runs`)
	rep, err = seed.Apply(ctx, core.SeedTarget(), docs, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rebuild) != 0 || rep.Run != nil || rep.Models != 0 || rep.Profiles != 0 || rep.Schedules != 0 {
		t.Fatalf("re-apply wrote %+v", rep)
	}
	if n := count(`SELECT count(*) FROM jobs.job_runs`); n != jobsBefore {
		t.Fatalf("re-apply enqueued %d jobs", n-jobsBefore)
	}
	if n := count(`SELECT count(*) FROM ingest.import_mappings WHERE version > 1`); n != 0 {
		t.Fatalf("re-apply bumped %d mapping versions", n)
	}

	// Changing one derivation rebuilds exactly that series (downstream is
	// the cascade's job), as version 2.
	changed := filepath.Join(t.TempDir(), "seed")
	copyDir(t, dir, changed)
	path := filepath.Join(changed, "series.yaml")
	body, _ := os.ReadFile(path)
	edited := strings.Replace(string(body), `expr: "(aaa - g) * 100"`, `expr: "(aaa - g) * 100.0"`, 1)
	if edited == string(body) {
		t.Fatal("AAA spread expression not found")
	}
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	docs2, err := seed.Validate(changed)
	if err != nil {
		t.Fatal(err)
	}
	rep, err = seed.Apply(ctx, core.SeedTarget(), docs2, uid)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rep.Rebuild, ",") != "spread.aaa_bps" {
		t.Fatalf("rebuild = %v", rep.Rebuild)
	}
	s, err = core.Catalog.Series(ctx, "spread.aaa_bps")
	if err != nil || s.Derivation == nil || s.Derivation.Version != 2 {
		t.Fatalf("spread.aaa_bps = %+v, %v", s, err)
	}
	if s.Layer != catalog.LayerDerived {
		t.Fatalf("layer = %s", s.Layer)
	}
}

func TestApplyRollsBackOnError(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	var uid int64
	if err := db.Owner.QueryRow(ctx, `INSERT INTO auth.users(email,name,role) VALUES ('lead@example.com','Lead','admin') RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	core, err := app.NewCore(db.Runtime, slog.New(slog.NewTextHandler(io.Discard, nil)), clock.System{})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "seed")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("sources.yaml", "- {key: computed, kind: computed, name: Computed}\n")
	write("entities.yaml", "- {key: l1.equity, kind: asset_class, name: Equity}\n")
	// Valid against the schema, but the derive registry rejects the params.
	write("series.yaml", `- {key: a, name: A, layer: raw, value_type: num, frequency: monthly}
- {key: b, name: B, layer: percentile, value_type: num, frequency: monthly,
   derivation: {fn: percentile_rank, inputs: {x: a}, params: {window_months: 0}, version: 1}}
`)
	docs, err := seed.Validate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Apply(ctx, core.SeedTarget(), docs, uid); err == nil || !strings.Contains(err.Error(), "series b") {
		t.Fatalf("want error for series b, got %v", err)
	}
	var n int
	if err := db.Owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM catalog.series) + (SELECT count(*) FROM catalog.entities) + (SELECT count(*) FROM catalog.sources)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d rows left after a failed apply", n)
	}
}

func copyDir(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}
