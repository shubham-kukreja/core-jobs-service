package datapoints_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"cloud.google.com/go/civil"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/testdb"
)

type anyFormula struct{}

func (anyFormula) ValidateDerivation(d catalog.Derivation) ([]string, error) {
	var keys []string
	for _, k := range d.Inputs {
		keys = append(keys, k)
	}
	return keys, nil
}

var (
	t0      = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	oct31   = civil.Date{Year: 2026, Month: 10, Day: 31}
	nov30   = civil.Date{Year: 2026, Month: 11, Day: 30}
	analyst = int64(1)
)

type env struct {
	t   *testing.T
	ctx context.Context
	db  *testdb.DB
	clk *clock.Fake
	cat *catalog.Store
	dp  *datapoints.Store
}

func setup(t *testing.T) *env {
	t.Helper()
	d := testdb.New(t)
	e := &env{t: t, ctx: context.Background(), db: d, clk: clock.NewFake(t0)}
	e.cat = catalog.New(d.Runtime, anyFormula{})
	e.dp = datapoints.New(d.Runtime, e.cat, e.clk)
	e.exec(`INSERT INTO auth.users(email,name,role) VALUES ('riya@example.com','Riya','analyst');
INSERT INTO catalog.sources(key,kind,name) VALUES
  ('override','override','Override'),('analyst','analyst','Analyst'),('file_import','file_import','File'),
  ('computed','computed','Computed'),('rbi','external_api','RBI')`)
	lo, hi := 0.0, 30.0
	e.saveSeries(catalog.SeriesInput{Key: "yield.gsec10y", Name: "10Y", Layer: catalog.LayerRaw, ValueType: catalog.ValueNum,
		Frequency: catalog.FreqMonthly, Validation: catalog.Validation{Min: &lo, Max: &hi}})
	e.saveSeries(catalog.SeriesInput{Key: "judg.rbi.stance", Name: "Stance", Layer: catalog.LayerJudgment, ValueType: catalog.ValueCat,
		Frequency: catalog.FreqMonthly, AllowedValues: []string{"cutting", "hold", "hiking"}})
	e.saveSeries(catalog.SeriesInput{Key: "pct.gsec", Name: "Pct", Layer: catalog.LayerPercentile, ValueType: catalog.ValueNum,
		Frequency: catalog.FreqMonthly, Derivation: &catalog.Derivation{Fn: "percentile_rank",
			Inputs: map[string]string{"x": "yield.gsec10y"}, Params: json.RawMessage(`{}`)}})
	return e
}

func (e *env) exec(sql string) {
	e.t.Helper()
	if _, err := e.db.Owner.Exec(e.ctx, sql); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) saveSeries(in catalog.SeriesInput) {
	e.t.Helper()
	if err := database.InTx(e.ctx, e.db.Runtime, func(tx pgx.Tx) error {
		_, err := e.cat.SaveSeries(e.ctx, tx, in)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) write(meta datapoints.WriteMeta, pts ...datapoints.Point) datapoints.WriteResult {
	e.t.Helper()
	var res datapoints.WriteResult
	if err := database.InTx(e.ctx, e.db.Runtime, func(tx pgx.Tx) error {
		var err error
		res, err = e.dp.Write(e.ctx, tx, meta, pts)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return res
}

func asAnalyst() datapoints.WriteMeta {
	return datapoints.WriteMeta{Source: "analyst", ActorID: &analyst}
}

func (e *env) count(sql string) int {
	e.t.Helper()
	var n int
	if err := e.db.Owner.QueryRow(e.ctx, sql).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func num(r *datapoints.Resolved) float64 {
	if r == nil || r.Value.Num == nil {
		return -1
	}
	return *r.Value.Num
}

func TestWriteValidatesAndEmitsOneEvent(t *testing.T) {
	e := setup(t)
	res := e.write(datapoints.WriteMeta{Source: "rbi"},
		datapoints.Point{SeriesKey: "yield.gsec10y", ObsDate: oct31, Value: datapoints.Num(6.72)},
		datapoints.Point{SeriesKey: "yield.gsec10y", ObsDate: nov30, Value: datapoints.Num(99)},          // out of range
		datapoints.Point{SeriesKey: "missing", ObsDate: oct31, Value: datapoints.Num(1)},                 // unknown
		datapoints.Point{SeriesKey: "judg.rbi.stance", ObsDate: oct31, Value: datapoints.Cat("cutting")}, // rbi can't write judgments
		datapoints.Point{SeriesKey: "yield.gsec10y", ObsDate: oct31, Value: datapoints.Num(6.8)},         // duplicate in batch
	)
	if res.Written != 1 || len(res.Rejected) != 4 || res.IDs[0] == 0 {
		t.Fatalf("result = %+v", res)
	}
	want := []string{datapoints.ReasonOutOfRange, datapoints.ReasonUnknownSeries, datapoints.ReasonNotWritable, datapoints.ReasonDuplicatePoint}
	for i, r := range res.Rejected {
		if r.Reason != want[i] || r.Index != i+1 {
			t.Errorf("rejection %d = %+v, want %s", i, r, want[i])
		}
	}
	if n := e.count(`SELECT count(*) FROM dp.outbox`); n != 1 {
		t.Fatalf("outbox rows = %d", n)
	}
	var payload []byte
	if err := e.db.Owner.QueryRow(e.ctx, `SELECT payload FROM dp.outbox`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var ev datapoints.WrittenEvent
	if err := json.Unmarshal(payload, &ev); err != nil || ev.BatchID != res.BatchID || ev.Origin != datapoints.OriginPrimary ||
		len(ev.SeriesIDs) != 1 || ev.MinDate != oct31 || ev.Layers[0] != "raw" {
		t.Fatalf("event = %+v (%v)", ev, err)
	}
}

func TestWriteSkipsNoOpsAndRollsBack(t *testing.T) {
	e := setup(t)
	p := datapoints.Point{SeriesKey: "yield.gsec10y", ObsDate: oct31, Value: datapoints.Num(6.72)}
	e.write(datapoints.WriteMeta{Source: "rbi"}, p)
	e.clk.Advance(time.Hour)
	again := e.write(datapoints.WriteMeta{Source: "rbi"}, p)
	if again.Written != 0 || again.Skipped != 1 {
		t.Fatalf("re-write = %+v", again)
	}
	if n := e.count(`SELECT count(*) FROM dp.outbox`); n != 1 {
		t.Fatalf("a no-op must not emit an event; outbox = %d", n)
	}

	// Computed rows skip on equal input hash, regardless of value.
	c := datapoints.Point{SeriesKey: "pct.gsec", ObsDate: oct31, Value: datapoints.Num(70), Inputs: []int64{1}, DerivVer: 1}
	first := e.write(datapoints.WriteMeta{Source: "computed", Origin: datapoints.OriginCascade}, c)
	c.Value = datapoints.Num(71)
	second := e.write(datapoints.WriteMeta{Source: "computed", Origin: datapoints.OriginCascade}, c)
	if first.Written != 1 || second.Skipped != 1 {
		t.Fatalf("computed: %+v / %+v", first, second)
	}

	// Rollback leaves neither rows nor events.
	before := e.count(`SELECT count(*) FROM dp.datapoints`) + e.count(`SELECT count(*) FROM dp.outbox`)
	boom := errors.New("boom")
	err := database.InTx(e.ctx, e.db.Runtime, func(tx pgx.Tx) error {
		if _, err := e.dp.Write(e.ctx, tx, datapoints.WriteMeta{Source: "rbi"},
			[]datapoints.Point{{SeriesKey: "yield.gsec10y", ObsDate: nov30, Value: datapoints.Num(6.9)}}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if after := e.count(`SELECT count(*) FROM dp.datapoints`) + e.count(`SELECT count(*) FROM dp.outbox`); after != before {
		t.Fatalf("rollback left rows: %d → %d", before, after)
	}
}

func TestWriteRequiresActorForAnalyst(t *testing.T) {
	e := setup(t)
	err := database.InTx(e.ctx, e.db.Runtime, func(tx pgx.Tx) error {
		_, err := e.dp.Write(e.ctx, tx, datapoints.WriteMeta{Source: "analyst"},
			[]datapoints.Point{{SeriesKey: "judg.rbi.stance", ObsDate: oct31, Value: datapoints.Cat("hold")}})
		return err
	})
	if !apperr.Is(err, apperr.InvalidParams) {
		t.Fatalf("want invalid_params, got %v", err)
	}
}

// TestPointInTime pins resolution: revisions, source priority, overrides and
// retractions, each seen only from the cutoff at which it was known.
func TestPointInTime(t *testing.T) {
	e := setup(t)
	at := func(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }

	e.write(datapoints.WriteMeta{Source: "rbi"}, datapoints.Point{SeriesKey: "yield.gsec10y", ObsDate: oct31, Value: datapoints.Num(6.72), KnownAt: at(0)})
	e.clk.Set(at(2))
	e.write(datapoints.WriteMeta{Source: "rbi"}, datapoints.Point{SeriesKey: "yield.gsec10y", ObsDate: oct31, Value: datapoints.Num(6.75)}) // revision
	e.clk.Set(at(4))
	ov := e.write(datapoints.WriteMeta{Source: "override", ActorID: &analyst},
		datapoints.Point{SeriesKey: "yield.gsec10y", ObsDate: oct31, Value: datapoints.Num(6.70), Note: "RBI print revised"})
	e.clk.Set(at(6))
	if err := database.InTx(e.ctx, e.db.Runtime, func(tx pgx.Tx) error {
		return e.dp.Retract(e.ctx, tx, asAnalyst(), ov.IDs[:1], "entered against wrong month")
	}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		cutoff time.Time
		want   float64
		source string
	}{
		{"before anything known", at(0).Add(-time.Second), -1, ""},
		{"first print", at(1), 6.72, "rbi"},
		{"revision", at(3), 6.75, "rbi"},
		{"override wins", at(5), 6.70, "override"},
		{"retracted override falls back", at(7), 6.75, "rbi"},
	}
	for _, c := range cases {
		r, err := e.dp.At(e.ctx, "yield.gsec10y", oct31, datapoints.ReadOpts{Cutoff: c.cutoff})
		if err != nil {
			t.Fatal(err)
		}
		if num(r) != c.want || (r != nil && r.Source != c.source) {
			t.Errorf("%s: got %v from %v, want %v from %s", c.name, num(r), r, c.want, c.source)
		}
	}

	// Restricting sources ignores the override even before its retraction.
	r, err := e.dp.At(e.ctx, "yield.gsec10y", oct31, datapoints.ReadOpts{Cutoff: at(5), Sources: []string{"rbi"}})
	if err != nil || num(r) != 6.75 {
		t.Fatalf("source filter: %v %v", r, err)
	}

	versions, err := e.dp.Versions(e.ctx, "yield.gsec10y", oct31)
	if err != nil || len(versions) != 4 || versions[0].Status != "retracted" || *versions[0].SupersedesID != ov.IDs[0] {
		t.Fatalf("versions = %+v, %v", versions, err)
	}

	// A retraction cannot be repeated.
	err = database.InTx(e.ctx, e.db.Runtime, func(tx pgx.Tx) error {
		return e.dp.Retract(e.ctx, tx, asAnalyst(), ov.IDs[:1], "again")
	})
	if !errors.Is(err, datapoints.ErrAlreadyRetracted) || !apperr.Is(err, apperr.Conflict) {
		t.Fatalf("double retraction: %v", err)
	}
	// Retracting emitted an event so the cascade recomputes.
	if n := e.count(`SELECT count(*) FROM dp.outbox`); n != 4 {
		t.Fatalf("outbox = %d, want 4 (3 writes + 1 retraction)", n)
	}
}

func TestRangeHistorySnapshotLineage(t *testing.T) {
	e := setup(t)
	var ids []int64
	for m := 1; m <= 6; m++ {
		d := civil.Date{Year: 2026, Month: time.Month(m), Day: 1}.AddMonths(1).AddDays(-1) // month ends
		res := e.write(datapoints.WriteMeta{Source: "rbi"}, datapoints.Point{SeriesKey: "yield.gsec10y", ObsDate: d,
			Value: datapoints.Num(6 + float64(m)/10), KnownAt: t0.Add(-time.Duration(7-m) * 24 * time.Hour)})
		ids = append(ids, res.IDs[0])
	}
	jun := civil.Date{Year: 2026, Month: 6, Day: 30}
	hist, err := e.dp.History(e.ctx, "yield.gsec10y", jun, 3, datapoints.ReadOpts{})
	if err != nil || len(hist) != 3 || hist[0].ObsDate.Month != 4 || hist[2].ObsDate != jun {
		t.Fatalf("history = %+v, %v", hist, err)
	}
	all, err := e.dp.History(e.ctx, "yield.gsec10y", jun, 0, datapoints.ReadOpts{})
	if err != nil || len(all) != 6 {
		t.Fatalf("unbounded history = %d, %v", len(all), err)
	}

	pct := e.write(datapoints.WriteMeta{Source: "computed", Origin: datapoints.OriginCascade},
		datapoints.Point{SeriesKey: "pct.gsec", ObsDate: jun, Value: datapoints.Num(80), Inputs: ids[3:], DerivVer: 1})
	lin, err := e.dp.Lineage(e.ctx, pct.IDs[0], 3)
	if err != nil || len(lin.Inputs) != 3 || lin.Datapoint.SeriesKey != "pct.gsec" || lin.Inputs[0].Datapoint.Source != "rbi" {
		t.Fatalf("lineage = %+v, %v", lin, err)
	}

	snap, err := e.dp.Snapshot(e.ctx, []string{"yield.gsec10y", "pct.gsec", "judg.rbi.stance", "missing"}, jun, datapoints.ReadOpts{})
	if err != nil || len(snap) != 2 || snap["pct.gsec"].DerivVer != 1 {
		t.Fatalf("snapshot = %+v, %v", snap, err)
	}

	b, err := e.dp.Batch(e.ctx, pct.BatchID)
	if err != nil || b.Origin != datapoints.OriginCascade || b.MinDate != jun || len(b.SeriesIDs) != 1 {
		t.Fatalf("batch = %+v, %v", b, err)
	}
	if _, err := e.dp.Get(e.ctx, 999999); !errors.Is(err, datapoints.ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}
}

func TestOutboxClaimAndDispatch(t *testing.T) {
	e := setup(t)
	for i := 0; i < 3; i++ {
		e.write(datapoints.WriteMeta{Source: "rbi"}, datapoints.Point{SeriesKey: "yield.gsec10y",
			ObsDate: civil.Date{Year: 2026, Month: time.Month(i + 1), Day: 28}, Value: datapoints.Num(6)})
	}
	// Two concurrent claimers never see the same rows.
	tx1, err := e.db.Runtime.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx1.Rollback(e.ctx) }()
	got1, err := e.dp.Claim(e.ctx, tx1, 2)
	if err != nil || len(got1) != 2 {
		t.Fatalf("claim 1 = %v, %v", got1, err)
	}
	err = database.InTx(e.ctx, e.db.Runtime, func(tx2 pgx.Tx) error {
		got2, err := e.dp.Claim(e.ctx, tx2, 10)
		if err != nil {
			return err
		}
		if len(got2) != 1 || got2[0].ID == got1[0].ID || got2[0].ID == got1[1].ID {
			t.Errorf("claim 2 overlapped: %v", got2)
		}
		return e.dp.MarkDispatched(e.ctx, tx2, []int64{got2[0].ID})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.dp.MarkDispatched(e.ctx, tx1, []int64{got1[0].ID, got1[1].ID}); err != nil {
		t.Fatal(err)
	}
	if err := tx1.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM dp.outbox WHERE dispatched_at IS NULL`); n != 0 {
		t.Fatalf("undispatched = %d", n)
	}
}

func TestRawPayloadRejectsCredentials(t *testing.T) {
	e := setup(t)
	err := database.InTx(e.ctx, e.db.Runtime, func(tx pgx.Tx) error {
		_, err := e.dp.Record(e.ctx, tx, datapoints.RawPayload{SourceKey: "rbi", FetchedAt: t0, BlobURI: "blob://b/k",
			SHA256: make([]byte, 32), Bytes: 10, Request: map[string]any{"url": "https://x", "api_key": "secret"}})
		return err
	})
	if !apperr.Is(err, apperr.InvalidParams) {
		t.Fatalf("want invalid_params, got %v", err)
	}
	err = database.InTx(e.ctx, e.db.Runtime, func(tx pgx.Tx) error {
		_, err := e.dp.Record(e.ctx, tx, datapoints.RawPayload{SourceKey: "rbi", FetchedAt: t0, BlobURI: "blob://b/k",
			SHA256: make([]byte, 32), Bytes: 10, Request: map[string]any{"url": "https://x"}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
