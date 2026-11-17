package derive_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"cloud.google.com/go/civil"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/derive"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/calendar"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/testdb"
)

type cenv struct {
	t    *testing.T
	ctx  context.Context
	db   *testdb.DB
	clk  *clock.Fake
	cat  *catalog.Store
	dp   *datapoints.Store
	casc derive.Cascade
}

func me(y, m int) civil.Date {
	return calendar.MonthEnd(civil.Date{Year: y, Month: time.Month(m), Day: 1})
}

// setupCascade seeds raw 10Y yields and P/E, and a chain:
// gap = 100/pe − y (expr) → pct.gap (percentile_rank) → score.gap (invert).
func setupCascade(t *testing.T) *cenv {
	t.Helper()
	d := testdb.New(t)
	e := &cenv{t: t, ctx: context.Background(), db: d, clk: clock.NewFake(time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))}
	reg := derive.NewDefaultRegistry()
	e.cat = catalog.New(d.Runtime, reg)
	e.dp = datapoints.New(d.Runtime, e.cat, e.clk)
	e.casc = derive.NewCascade(derive.CascadeDeps{
		DB: d.Runtime, Catalog: e.cat, Reader: e.dp, Writer: e.dp, Batches: e.dp, Registry: reg, Clock: e.clk,
		Governance: derive.Governance{PercentileMinObservations: 3},
	})
	if _, err := d.Owner.Exec(e.ctx, `INSERT INTO catalog.sources(key,kind,name) VALUES
  ('file_import','file_import','File'),('computed','computed','Computed')`); err != nil {
		t.Fatal(err)
	}
	num := func(key string, layer catalog.Layer, der *catalog.Derivation) {
		e.save(catalog.SeriesInput{Key: key, Name: key, Layer: layer, ValueType: catalog.ValueNum,
			Frequency: catalog.FreqMonthly, Derivation: der})
	}
	num("yield.gsec10y", catalog.LayerRaw, nil)
	num("pe.nifty50", catalog.LayerRaw, nil)
	num("gap.ey", catalog.LayerDerived, &catalog.Derivation{Fn: "expr", Version: 1,
		Inputs: map[string]string{"pe": "pe.nifty50", "y": "yield.gsec10y"}, Params: json.RawMessage(`{"expr":"100 / pe - y"}`)})
	num("pct.gap.ey", catalog.LayerPercentile, &catalog.Derivation{Fn: "percentile_rank", Version: 1,
		Inputs: map[string]string{"x": "gap.ey"}, Params: json.RawMessage(`{"window_months":180,"min_obs":3}`)})
	num("score.gap.ey", catalog.LayerScore, &catalog.Derivation{Fn: "invert", Version: 1,
		Inputs: map[string]string{"x": "pct.gap.ey"}})
	return e
}

func (e *cenv) save(in catalog.SeriesInput) {
	e.t.Helper()
	if err := database.InTx(e.ctx, e.db.Runtime, func(tx pgx.Tx) error {
		_, err := e.cat.SaveSeries(e.ctx, tx, in)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *cenv) write(pts ...datapoints.Point) datapoints.WriteResult {
	e.t.Helper()
	var res datapoints.WriteResult
	if err := database.InTx(e.ctx, e.db.Runtime, func(tx pgx.Tx) error {
		var err error
		res, err = e.dp.Write(e.ctx, tx, datapoints.WriteMeta{Source: "file_import", Origin: datapoints.OriginPrimary}, pts)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	if len(res.Rejected) > 0 {
		e.t.Fatalf("rejected: %+v", res.Rejected)
	}
	return res
}

func (e *cenv) at(key string, d civil.Date) *float64 {
	e.t.Helper()
	r, err := e.dp.At(e.ctx, key, d, datapoints.ReadOpts{})
	if err != nil {
		e.t.Fatal(err)
	}
	if r == nil {
		return nil
	}
	return r.Value.Num
}

func (e *cenv) run(req derive.CascadeRequest) derive.CascadeResult {
	e.t.Helper()
	res, err := e.casc.Run(e.ctx, req, 0, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

func pt(key string, d civil.Date, v float64) datapoints.Point {
	return datapoints.Point{SeriesKey: key, ObsDate: d, Value: datapoints.Num(v)}
}

func TestCascadeIncrementalChainAndIdempotence(t *testing.T) {
	e := setupCascade(t)
	var pts []datapoints.Point
	pes := []float64{20, 25, 18, 22} // Jan..Apr 2026
	for i, pe := range pes {
		pts = append(pts, pt("pe.nifty50", me(2026, i+1), pe), pt("yield.gsec10y", me(2026, i+1), 2))
	}
	w := e.write(pts...)

	res := e.run(derive.CascadeRequest{Mode: derive.ModeIncremental, BatchID: w.BatchID})
	// gap: 4 rows; pct: Mar, Apr (min_obs 3); score: Mar, Apr.
	if res.Series != 3 || res.RowsWritten != 8 {
		t.Fatalf("first run: %+v", res)
	}
	if g := e.at("gap.ey", me(2026, 1)); g == nil || *g != 3 {
		t.Fatalf("gap Jan = %v, want 3", g)
	}
	// Mar gap = 100/18−2 = 3.56 is the max of Jan..Mar → 100; score 0.
	if p := e.at("pct.gap.ey", me(2026, 3)); p == nil || *p != 100 {
		t.Fatalf("pct Mar = %v", p)
	}
	if s := e.at("score.gap.ey", me(2026, 3)); s == nil || *s != 0 {
		t.Fatalf("score Mar = %v", s)
	}
	if e.at("pct.gap.ey", me(2026, 2)) != nil {
		t.Fatal("pct below min_obs must not be written")
	}

	again := e.run(derive.CascadeRequest{Mode: derive.ModeIncremental, BatchID: w.BatchID})
	if again.RowsWritten != 0 {
		t.Fatalf("re-run wrote %d rows, want 0", again.RowsWritten)
	}

	// A revision of Jan moves every later percentile window.
	e.clk.Advance(time.Hour)
	rev := e.write(pt("pe.nifty50", me(2026, 1), 10)) // gap Jan = 8, new max
	res = e.run(derive.CascadeRequest{Mode: derive.ModeIncremental, BatchID: rev.BatchID})
	if p := e.at("pct.gap.ey", me(2026, 3)); p == nil || *p != 50 {
		t.Fatalf("pct Mar after Jan revision = %v, want 50", p)
	}
	if res.RowsWritten == 0 {
		t.Fatal("revision wrote nothing")
	}
}

func TestCascadeRebuildKeepsOldRows(t *testing.T) {
	e := setupCascade(t)
	w := e.write(pt("pe.nifty50", me(2026, 1), 20), pt("yield.gsec10y", me(2026, 1), 2))
	e.run(derive.CascadeRequest{Mode: derive.ModeIncremental, BatchID: w.BatchID})
	before := e.clk.Now()

	e.clk.Advance(time.Hour)
	e.save(catalog.SeriesInput{Key: "gap.ey", Name: "gap.ey", Layer: catalog.LayerDerived, ValueType: catalog.ValueNum,
		Frequency: catalog.FreqMonthly, Derivation: &catalog.Derivation{Fn: "expr", Version: 2,
			Inputs: map[string]string{"pe": "pe.nifty50", "y": "yield.gsec10y"}, Params: json.RawMessage(`{"expr":"100 / pe"}`)}})
	res := e.run(derive.CascadeRequest{Mode: derive.ModeRebuild, SeriesKeys: []string{"gap.*"}, IncludeDownstream: true})
	if res.RowsWritten != 1 {
		t.Fatalf("rebuild: %+v", res)
	}
	if g := e.at("gap.ey", me(2026, 1)); g == nil || *g != 5 {
		t.Fatalf("gap now = %v, want 5", g)
	}
	old, err := e.dp.At(e.ctx, "gap.ey", me(2026, 1), datapoints.ReadOpts{Cutoff: before})
	if err != nil || old == nil || *old.Value.Num != 3 || old.DerivVer != 1 {
		t.Fatalf("old row at earlier cutoff = %+v, %v", old, err)
	}
	if again := e.run(derive.CascadeRequest{Mode: derive.ModeRebuild, SeriesKeys: []string{"gap.ey"}}); again.RowsWritten != 0 {
		t.Fatalf("second rebuild wrote %d", again.RowsWritten)
	}
}

func TestCascadeSkipsCascadeOrigin(t *testing.T) {
	args, err := derive.Subscriber{}.Handle(context.Background(), eventOf(t, datapoints.WrittenEvent{SeriesIDs: []int64{1}, MinDate: me(2026, 1), MaxDate: me(2026, 1), Origin: datapoints.OriginCascade}))
	if err != nil || len(args) != 0 {
		t.Fatalf("cascade-origin event enqueued %v, %v", args, err)
	}
	args, err = derive.Subscriber{}.Handle(context.Background(), eventOf(t, datapoints.WrittenEvent{SeriesIDs: []int64{1}, MinDate: me(2026, 1), MaxDate: me(2026, 1), Origin: datapoints.OriginPrimary}))
	if err != nil || len(args) != 1 || args[0].(derive.CascadeArgs).Mode != derive.ModeIncremental {
		t.Fatalf("primary event: %v, %v", args, err)
	}
}

func eventOf(t *testing.T, w datapoints.WrittenEvent) jobs.Event {
	t.Helper()
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	return jobs.Event{Topic: datapoints.TopicWritten, Payload: b}
}
