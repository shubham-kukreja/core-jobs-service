package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"cloud.google.com/go/civil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	"github.com/shubham-kukreja/core-jobs-service/internal/model"
	"github.com/shubham-kukreja/core-jobs-service/internal/model/modeltest"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
)

// TestG3GoldenAndWhatIf is Gate G3: the workbook's scores go in as
// datapoints, model.run (through the queue and worker) publishes an
// allocation that matches every sheet output, and a what-if run with a
// draft config returns a different allocation without touching published
// state.
func TestG3GoldenAndWhatIf(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	g := setupG2(ctx, t)
	asOf := civil.Date{Year: 2026, Month: 3, Day: 31}

	// 1. The scoring sheets' inputs, as score overrides, and Small Cap's
	// earnings-collapse trigger. The trigger write enqueues veto.evaluate.
	cv, err := g.core.Model.Active(ctx, model.DefaultModel)
	if err != nil {
		t.Fatal(err)
	}
	signals := map[string][]model.Signal{}
	for _, sc := range cv.Config.Scorecards {
		for _, b := range sc.Buckets {
			signals[b] = sc.Signals
		}
	}
	for _, sec := range cv.Config.SectorSatellite.Sectors {
		signals[sec] = cv.Config.SectorSatellite.Signals
	}
	known := time.Date(2026, 4, 5, 0, 0, 0, 0, time.UTC)
	var pts []datapoints.Point
	for b, vals := range modeltest.Scores {
		for i, sg := range signals[b] {
			pts = append(pts, datapoints.Point{SeriesKey: b + "::" + sg.Key, ObsDate: asOf, Value: datapoints.Num(vals[i]), KnownAt: known})
		}
	}
	pts = append(pts, datapoints.Point{SeriesKey: modeltest.VetoTrigger, ObsDate: asOf, Value: datapoints.Bool(true), KnownAt: known})
	err = database.InTx(ctx, g.db.Runtime, func(tx pgx.Tx) error {
		res, err := g.core.Datapoints.Write(ctx, tx, datapoints.WriteMeta{Source: "override", ActorID: &g.uid}, pts)
		if err == nil && len(res.Rejected) > 0 {
			err = errors.New(res.Rejected[0].SeriesKey + ": " + res.Rejected[0].Reason)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	g.drain(ctx, t)
	if r, err := g.core.Datapoints.At(ctx, model.VetoSeries("equity.small"), asOf, datapoints.ReadOpts{}); err != nil || r == nil || r.Value.Bool == nil || !*r.Value.Bool {
		t.Fatalf("veto.equity.small = %+v, %v", r, err)
	}

	// 2. The golden run, published through the queue.
	published := g.modelRun(ctx, t, model.RunArgs{Model: "maaf", AsOf: asOf, Params: model.Params{SectorsHeld: modeltest.SectorsHeld}, Publish: true})
	if published.Status != model.RunPublished || len(published.Warnings) != 0 {
		t.Fatalf("published run = %+v", published)
	}
	values, err := g.core.Model.RunValues(ctx, published.UUID, "")
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]model.Node{}
	for _, n := range values {
		byKey[n.Key] = n
	}
	for key, want := range modeltest.Outputs {
		if n := byKey[key]; n.Num == nil || math.Abs(*n.Num-want) > 1e-9 {
			t.Errorf("%s = %v, workbook %v", key, n.Num, want)
		}
	}
	pubAlloc := g.allocation(ctx, t, published.UUID)
	for _, l := range modeltest.Lines {
		if got := pubAlloc[l.Item]; math.Abs(got.InexactFloat64()-l.Weight) > 0.0005 {
			t.Errorf("%s = %s, workbook %.10f", l.Item, got, l.Weight)
		}
	}

	// 3. What-if: a draft config with a wider L1 cap and a lower sector bar.
	var draft model.ConfigVersion
	err = database.InTx(ctx, g.db.Runtime, func(tx pgx.Tx) error {
		draft, err = g.core.Model.Create(ctx, tx, "maaf", 0, []model.PatchOp{
			{Op: "replace", Path: "/scorecards/l1/tilt_cap", Value: json.RawMessage(`0.4`)},
			{Op: "replace", Path: "/sector_satellite/qualify_threshold", Value: json.RawMessage(`55`)},
		}, "what-if", g.uid)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	v := draft.Version
	whatIf := g.modelRun(ctx, t, model.RunArgs{Model: "maaf", AsOf: asOf, ConfigVersion: &v,
		Params: model.Params{SectorsHeld: []string{"sector.capgoods", "sector.pharma"}}})
	if whatIf.Status != model.RunDraft || whatIf.ConfigVersion != draft.Version {
		t.Fatalf("what-if run = %+v", whatIf)
	}
	wiAlloc := g.allocation(ctx, t, whatIf.UUID)
	if wiAlloc["sector.pharma"].IsZero() || wiAlloc["debt.gilt"].Equal(pubAlloc["debt.gilt"]) {
		t.Fatalf("what-if allocation did not change: %v", wiAlloc)
	}

	// Published state is untouched.
	cur, err := g.core.Model.CurrentRun(ctx, "maaf", "balanced")
	if err != nil || cur.UUID != published.UUID {
		t.Fatalf("current run = %v, %v", cur.UUID, err)
	}
	for k, w := range g.allocation(ctx, t, cur.UUID) {
		if !w.Equal(pubAlloc[k]) {
			t.Errorf("published %s moved: %s → %s", k, pubAlloc[k], w)
		}
	}
	if active, _ := g.core.Model.Active(ctx, "maaf"); active.Version != cv.Version {
		t.Fatalf("active config moved to v%d", active.Version)
	}
}

// modelRun enqueues model.run, waits, and returns the stored run.
func (g *g2) modelRun(ctx context.Context, t *testing.T, a model.RunArgs) model.Run {
	t.Helper()
	ref, err := g.core.Jobs.Enqueue(ctx, a, jobs.WithTrigger(jobs.TriggerUser), jobs.WithUser(g.uid))
	if err != nil {
		t.Fatal(err)
	}
	g.drain(ctx, t)
	var result []byte
	if err := g.db.Owner.QueryRow(ctx, `SELECT result FROM jobs.job_runs WHERE uuid = $1`, ref.UUID).Scan(&result); err != nil {
		t.Fatal(err)
	}
	var res struct {
		RunID uuid.UUID `json:"run_id"`
	}
	if err := json.Unmarshal(result, &res); err != nil {
		t.Fatalf("result %s: %v", result, err)
	}
	run, err := g.core.Model.GetRun(ctx, res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.JobRunID == nil {
		t.Fatal("run does not link its job")
	}
	return run
}

func (g *g2) allocation(ctx context.Context, t *testing.T, id uuid.UUID) map[string]decimal.Decimal {
	t.Helper()
	lines, err := g.core.Model.Allocation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]decimal.Decimal{}
	total := decimal.Zero
	for _, l := range lines {
		out[l.LineItem] = l.PortfolioWeight
		total = total.Add(l.PortfolioWeight)
	}
	if !total.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("allocation sums to %s", total)
	}
	return out
}
