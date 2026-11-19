package model_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/civil"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/shubham-kukreja/core-jobs-service/internal/app"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	"github.com/shubham-kukreja/core-jobs-service/internal/model"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/testdb"
	"github.com/shubham-kukreja/core-jobs-service/internal/seed"
)

var (
	mar31   = civil.Date{Year: 2026, Month: 3, Day: 31}
	jun30   = civil.Date{Year: 2026, Month: 6, Day: 30}
	knownAt = time.Date(2026, 4, 5, 0, 0, 0, 0, time.UTC)
	now     = time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
)

type env struct {
	ctx  context.Context
	t    *testing.T
	db   *testdb.DB
	core *app.Core
	deps model.JobDeps
	uid  int64
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	db := testdb.New(t)
	var uid int64
	if err := db.Owner.QueryRow(ctx, `INSERT INTO auth.users(email,name,role) VALUES ('lead@example.com','Lead','admin') RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	core, err := app.NewCore(db.Runtime, slog.New(slog.NewTextHandler(io.Discard, nil)), clock.NewFake(now))
	if err != nil {
		t.Fatal(err)
	}
	docs, err := seed.Validate(filepath.Join("..", "..", "configs", "seed"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Apply(ctx, core.SeedTarget(), docs, uid); err != nil {
		t.Fatal(err)
	}
	return &env{ctx: ctx, t: t, db: db, core: core, uid: uid, deps: model.JobDeps{
		DB: db.Runtime, Store: core.Model, Engine: core.Engine, Loader: core.Loader,
		Reader: core.Datapoints, Writer: core.Datapoints, Clock: core.Clock,
	}}
}

// writeWorkbook writes the workbook's scoring-sheet inputs as score
// overrides observed on d, and Small Cap's earnings-collapse trigger.
func (e *env) writeWorkbook(d civil.Date) {
	e.t.Helper()
	cv, err := e.core.Model.Active(e.ctx, "maaf")
	if err != nil {
		e.t.Fatal(err)
	}
	signals := func(b string) []model.Signal {
		for _, sc := range cv.Config.Scorecards {
			for _, x := range sc.Buckets {
				if x == b {
					return sc.Signals
				}
			}
		}
		return cv.Config.SectorSatellite.Signals
	}
	var pts []datapoints.Point
	for b, vals := range model.WorkbookScores {
		for i, sg := range signals(b) {
			pts = append(pts, datapoints.Point{SeriesKey: b + "::" + sg.Key, ObsDate: d, Value: datapoints.Num(vals[i]), KnownAt: knownAt})
		}
	}
	pts = append(pts, datapoints.Point{SeriesKey: "trig.eps_collapse.equity.small", ObsDate: d, Value: datapoints.Bool(true), KnownAt: knownAt})
	e.write("override", pts)
}

func (e *env) write(source string, pts []datapoints.Point) {
	e.t.Helper()
	err := database.InTx(e.ctx, e.db.Runtime, func(tx pgx.Tx) error {
		res, err := e.core.Datapoints.Write(e.ctx, tx, datapoints.WriteMeta{Source: source, ActorID: &e.uid}, pts)
		if err != nil {
			return err
		}
		if len(res.Rejected) > 0 {
			return errors.New(res.Rejected[0].SeriesKey + ": " + res.Rejected[0].Reason + " " + res.Rejected[0].Message)
		}
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) run(a model.RunArgs) (model.Run, model.Result) {
	e.t.Helper()
	run, res, err := model.RunModel(e.ctx, e.deps, a, nil, &e.uid)
	if err != nil {
		e.t.Fatal(err)
	}
	return run, res
}

func nodes(t *testing.T, s *model.Store, run model.Run) map[string]model.Node {
	t.Helper()
	vals, err := s.RunValues(context.Background(), run.UUID, "")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]model.Node{}
	for _, n := range vals {
		out[n.Key] = n
	}
	return out
}

// TestModelRunGolden is P3-T7-04 through the database: the workbook's
// scores go in as datapoints, model.run reads them point in time, and the
// stored run reproduces every sheet output.
func TestModelRunGolden(t *testing.T) {
	e := setup(t)
	e.writeWorkbook(mar31)
	held := model.Params{SectorsHeld: []string{"sector.capgoods"}}
	run, res := e.run(model.RunArgs{Model: "maaf", AsOf: mar31, Params: held, Publish: true})
	if run.Status != model.RunPublished || run.ConfigVersion != 1 || run.RiskProfileKey != "balanced" || len(res.Warnings) != 0 {
		t.Fatalf("run = %+v, warnings %+v", run, res.Warnings)
	}
	stored := nodes(t, e.core.Model, run)
	for key, want := range model.WorkbookOutputs {
		n, ok := stored[key]
		if !ok || n.Num == nil || math.Abs(*n.Num-want) > 1e-9 {
			t.Errorf("%s = %v, workbook %v", key, n.Num, want)
		}
	}
	if n := stored["equity.small::veto"]; n.Bool == nil || !*n.Bool || !strings.Contains(n.Formula, "trig.eps_collapse.equity.small") {
		t.Errorf("small veto = %+v", n)
	}
	if n := stored["input:l1.debt::valuation"]; n.Num == nil || *n.Num != 65 || n.Step != "input" || len(n.Inputs.Datapoints) != 1 {
		t.Errorf("input node = %+v", n)
	}
	alloc, err := e.core.Model.Allocation(e.ctx, run.UUID)
	if err != nil {
		t.Fatal(err)
	}
	want := model.WorkbookPortfolio
	total := decimal.Zero
	for _, l := range alloc {
		if d := math.Abs(l.PortfolioWeight.InexactFloat64() - want[l.LineItem]); d > 0.0005 {
			t.Errorf("%s = %s, workbook %v", l.LineItem, l.PortfolioWeight, want[l.LineItem])
		}
		total = total.Add(l.PortfolioWeight)
	}
	if len(alloc) != len(want) || !total.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("%d lines summing to %s", len(alloc), total)
	}

	// Reproducible from stored inputs: replay gives the same hash and nodes.
	in, err := e.core.Model.Replay(e.ctx, run.UUID)
	if err != nil {
		t.Fatal(err)
	}
	h, err := model.InputHash(in)
	if err != nil || string(h) != string(run.DerivationHash) {
		t.Fatalf("replay hash differs: %x vs %x (%v)", h, run.DerivationHash, err)
	}
	again, err := e.core.Engine.Evaluate(e.ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	for k, n := range again.Nodes {
		s := stored[k]
		if !reflect.DeepEqual(n.Num, s.Num) || !reflect.DeepEqual(n.Bool, s.Bool) {
			t.Errorf("replay %s = %v/%v, stored %v/%v", k, n.Num, n.Bool, s.Num, s.Bool)
		}
	}

	// Point in time: with a cutoff before the scores were known, every
	// score is missing and scores 50 with a warning.
	early := knownAt.Add(-time.Hour)
	_, res = e.run(model.RunArgs{Model: "maaf", AsOf: mar31, Cutoff: &early})
	if len(res.Warnings) != 85 || res.Warnings[0].Code != "missing_score" {
		t.Fatalf("early cutoff warnings = %d", len(res.Warnings))
	}
}

func TestWhatIfLifecycleAndOverrides(t *testing.T) {
	e := setup(t)
	e.writeWorkbook(mar31)
	held := model.Params{SectorsHeld: []string{"sector.capgoods"}}
	pub, _ := e.run(model.RunArgs{Model: "maaf", AsOf: mar31, Params: held, Publish: true})
	pubAlloc, _ := e.core.Model.Allocation(e.ctx, pub.UUID)

	// A draft config (L1 cap 0.20 → 0.40) is a what-if: a different
	// allocation, published state untouched, and it cannot be published.
	var v2 model.ConfigVersion
	err := database.InTx(e.ctx, e.db.Runtime, func(tx pgx.Tx) error {
		var err error
		v2, err = e.core.Model.Create(e.ctx, tx, "maaf", 0, []model.PatchOp{
			{Op: "test", Path: "/scorecards/l1/tilt_cap", Value: json.RawMessage(`0.2`)},
			{Op: "replace", Path: "/scorecards/l1/tilt_cap", Value: json.RawMessage(`0.4`)},
		}, "what-if: wider L1 cap", e.uid)
		return err
	})
	if err != nil || v2.Version != 2 || v2.Status != model.ConfigDraft {
		t.Fatalf("v2 = %+v, %v", v2, err)
	}
	two := 2
	whatIf, _ := e.run(model.RunArgs{Model: "maaf", AsOf: mar31, ConfigVersion: &two, Params: held})
	wiAlloc, _ := e.core.Model.Allocation(e.ctx, whatIf.UUID)
	if whatIf.Status != model.RunDraft || whatIf.ConfigVersion != 2 || reflect.DeepEqual(pubAlloc, wiAlloc) {
		t.Fatalf("what-if = %+v", whatIf)
	}
	if got, _ := e.core.Model.GetRun(e.ctx, pub.UUID); got.Status != model.RunPublished {
		t.Fatalf("published run is now %s", got.Status)
	}
	if active, _ := e.core.Model.Active(e.ctx, "maaf"); active.Version != 1 {
		t.Fatalf("active config is v%d", active.Version)
	}
	_, _, err = model.RunModel(e.ctx, e.deps, model.RunArgs{Model: "maaf", AsOf: mar31, ConfigVersion: &two, Params: held, Publish: true}, nil, &e.uid)
	if apperr.CodeOf(err) != apperr.InvalidParams {
		t.Fatalf("publishing a draft config: %v", err)
	}
	ops, err := e.core.Model.Diff(e.ctx, "maaf", 1, 2)
	if err != nil || len(ops) != 1 || ops[0].Path != "/scorecards/l1/tilt_cap" || string(ops[0].Value) != "0.4" {
		t.Fatalf("diff = %+v, %v", ops, err)
	}

	// Lifecycle: approve the draft, publish it, the old one is superseded.
	tx := func(f func(tx pgx.Tx) error) error { return database.InTx(e.ctx, e.db.Runtime, f) }
	draft, _ := e.run(model.RunArgs{Model: "maaf", AsOf: mar31, Params: held})
	if err := tx(func(tx pgx.Tx) error { return e.core.Model.Approve(e.ctx, tx, draft.UUID, e.uid, "") }); err != nil {
		t.Fatal(err)
	}
	if err := tx(func(tx pgx.Tx) error { return e.core.Model.Approve(e.ctx, tx, draft.UUID, e.uid, "") }); !errors.Is(err, model.ErrInvalidState) {
		t.Fatalf("second approve: %v", err)
	}
	if err := tx(func(tx pgx.Tx) error { return e.core.Model.Publish(e.ctx, tx, draft.UUID, e.uid) }); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.core.Model.GetRun(e.ctx, pub.UUID); got.Status != model.RunSuperseded {
		t.Fatalf("old published run is %s", got.Status)
	}
	if got, _ := e.core.Model.GetRun(e.ctx, draft.UUID); got.Status != model.RunPublished || got.ApprovedBy == nil {
		t.Fatalf("new run = %+v", got)
	}
	page, err := e.core.Model.ListRuns(e.ctx, model.RunFilter{Model: "maaf", Status: model.RunPublished})
	if err != nil || len(page.Items) != 1 || page.Items[0].UUID != draft.UUID {
		t.Fatalf("published runs = %+v, %v", page.Items, err)
	}

	// A node override pins the metals veto; the run marks it and the tilt
	// cannot be positive.
	var ov model.NodeOverride
	if err := tx(func(tx pgx.Tx) error {
		var err error
		ov, err = e.core.Model.CreateOverride(e.ctx, tx, "maaf", model.NodeOverride{NodeKey: "l1.metals::veto", AsOf: mar31,
			Value: datapoints.Bool(true), Reason: "gold froth", ActorID: e.uid})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ovRun, _ := e.run(model.RunArgs{Model: "maaf", AsOf: mar31, Params: held})
	n := nodes(t, e.core.Model, ovRun)
	if v := n["l1.metals::veto"]; !v.IsOverride || v.Bool == nil || !*v.Bool || *n["l1.metals::tilt"].Num > 0 {
		t.Fatalf("override = %+v, tilt %v", v, *n["l1.metals::tilt"].Num)
	}
	if err := tx(func(tx pgx.Tx) error { return e.core.Model.DeleteOverride(e.ctx, tx, ov.ID, e.uid) }); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.core.Model.ActiveOverrides(e.ctx, "maaf", mar31); len(got) != 0 {
		t.Fatalf("deleted override still active: %+v", got)
	}

	// Satellite history: Capital Goods was held in the published run, so
	// the next quarter's load counts one quarter held.
	in, err := e.core.Loader.Load(e.ctx, model.LoadRequest{Model: "maaf", AsOf: jun30, Config: v2.Config, Params: held})
	if err != nil || in.History.QuartersHeld["sector.capgoods"] != 1 {
		t.Fatalf("history = %+v, %v", in.History, err)
	}
	// Scores are read as of: March's are the latest at April 30, but at
	// June 30 they are outside the 3-month lookback and count as missing.
	if _, ok := in.Inputs["l1.debt::valuation"]; ok {
		t.Fatal("a March score was used at June 30")
	}
	apr := civil.Date{Year: 2026, Month: 4, Day: 30}
	in, err = e.core.Loader.Load(e.ctx, model.LoadRequest{Model: "maaf", AsOf: apr, Config: v2.Config, Params: held})
	if inp, ok := in.Inputs["l1.debt::valuation"]; err != nil || !ok || *inp.ObservedAt != mar31 || *inp.Value.Num != 65 {
		t.Fatalf("as-of input = %+v, %v", inp, err)
	}
}

func TestVetoEvaluate(t *testing.T) {
	e := setup(t)
	e.writeWorkbook(mar31)
	res, err := model.EvaluateVetoes(e.ctx, e.deps, model.VetoArgs{AsOf: &mar31}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Active, []string{"equity.small"}) || !reflect.DeepEqual(res.NewlyActive, []string{"equity.small"}) || res.Written != 1 {
		t.Fatalf("veto = %+v", res)
	}
	r, err := e.core.Datapoints.At(e.ctx, "veto.equity.small", mar31, datapoints.ReadOpts{})
	if err != nil || r == nil || r.Value.Bool == nil || !*r.Value.Bool || len(r.Inputs) != 1 || !r.KnownAt.Equal(knownAt) {
		t.Fatalf("veto row = %+v, %v", r, err)
	}
	// Re-running writes nothing new.
	res, err = model.EvaluateVetoes(e.ctx, e.deps, model.VetoArgs{AsOf: &mar31}, nil)
	if err != nil || res.Written != 0 {
		t.Fatalf("re-run = %+v, %v", res, err)
	}
	// The trigger clears in April: the veto is written false.
	apr := civil.Date{Year: 2026, Month: 4, Day: 30}
	e.write("override", []datapoints.Point{{SeriesKey: "trig.eps_collapse.equity.small", ObsDate: apr, Value: datapoints.Bool(false), KnownAt: knownAt.AddDate(0, 1, 0)}})
	res, err = model.EvaluateVetoes(e.ctx, e.deps, model.VetoArgs{AsOf: &apr}, nil)
	if err != nil || len(res.Active) != 0 || res.Written != 1 {
		t.Fatalf("cleared = %+v, %v", res, err)
	}

	// Trigger writes enqueue veto.evaluate for their date.
	args, err := model.VetoSubscriber{}.Handle(e.ctx, jobs.Event{Topic: datapoints.TopicWritten,
		Payload: json.RawMessage(`{"layers":["score","trigger"],"max_date":"2026-04-30"}`)})
	if err != nil || len(args) != 1 || *args[0].(model.VetoArgs).AsOf != apr {
		t.Fatalf("subscriber = %+v, %v", args, err)
	}
	if args, _ := (model.VetoSubscriber{}).Handle(e.ctx, jobs.Event{Payload: json.RawMessage(`{"layers":["raw"]}`)}); len(args) != 0 {
		t.Fatal("raw writes enqueued veto.evaluate")
	}
}
