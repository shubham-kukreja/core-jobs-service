package model

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
)

func eval(t *testing.T, in EvalInput) Result {
	t.Helper()
	res, err := NewEngine().Evaluate(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func wantInvalid(t *testing.T, err error, reason string) {
	t.Helper()
	e, ok := apperr.As(err)
	if !ok || e.Code != apperr.InvalidParams || len(e.Details) == 0 || e.Details[0].Reason != reason {
		t.Fatalf("want invalid_params/%s, got %v", reason, err)
	}
}

func num(n Node) float64 { return *n.Num }

func TestPlanWorkedExample(t *testing.T) {
	// Plan: Debt C = 57.5, k = 20% → t = 3.0%; p = 35% × 1.03 = 36.05%;
	// Large Cap core = 46.6% × 0.925 = 43.1%.
	res := eval(t, workbookInput(t))
	if math.Abs(num(res.Nodes["l1.debt::tilt"])-0.03) > 1e-12 || math.Abs(num(res.Nodes["l1.debt::prelim"])-0.3605) > 1e-12 {
		t.Fatalf("debt tilt %v prelim %v", num(res.Nodes["l1.debt::tilt"]), num(res.Nodes["l1.debt::prelim"]))
	}
	if got := num(res.Nodes["equity.large::within"]); math.Abs(got-0.431) > 0.0005 {
		t.Fatalf("large cap core = %v", got)
	}
	if f := res.Nodes["l1.debt::composite"].Formula; f != "0.3×65 + 0.2×60 + 0.2×55 + 0.15×55 + 0.15×45 = 57.5" {
		t.Fatalf("formula = %q", f)
	}
	if len(res.Nodes["l1.debt::composite"].Inputs.Datapoints) != 5 {
		t.Fatalf("lineage = %v", res.Nodes["l1.debt::composite"].Inputs)
	}
}

func TestVetoBlocksOverweightOnly(t *testing.T) {
	in := workbookInput(t)
	// Debt is overweight (C 57.5); a credit event vetoes corporate (C 55.5).
	in.Inputs["trig.credit_event"] = Input{Value: datapoints.Bool(true)}
	res := eval(t, in)
	if !res.Nodes["debt.corporate::veto"].IsTrue() || num(res.Nodes["debt.corporate::tilt"]) != 0 {
		t.Fatalf("vetoed corporate tilt = %v", num(res.Nodes["debt.corporate::tilt"]))
	}
	if !strings.Contains(res.Nodes["debt.corporate::veto"].Formula, "trig.credit_event") {
		t.Fatal(res.Nodes["debt.corporate::veto"].Formula)
	}
	// Small Cap is underweight and vetoed: the veto never forces it lower.
	if num(res.Nodes["equity.small::tilt"]) != tiltOf(33.25, 0.30, false) {
		t.Fatal("veto changed an underweight")
	}
}

func TestMissingScoreUsesDefaultAndWarns(t *testing.T) {
	in := workbookInput(t)
	delete(in.Inputs, "l1.metals::momentum")
	in.Inputs["l1.metals::flows"] = Input{Value: datapoints.Value{}} // null
	res := eval(t, in)
	want := []Warning{
		{Code: "missing_score", Series: "l1.metals::flows", Node: "l1.metals", Used: 50},
		{Code: "missing_score", Series: "l1.metals::momentum", Node: "l1.metals", Used: 50},
	}
	if !reflect.DeepEqual(res.Warnings, want) {
		t.Fatalf("warnings = %+v", res.Warnings)
	}
	// 0.3×35 + 0.2×60 + 0.2×65 + 0.15×50 + 0.15×50
	if got := num(res.Nodes["l1.metals::composite"]); math.Abs(got-50.5) > 1e-12 {
		t.Fatalf("composite = %v", got)
	}
}

func TestOverridesPinNodes(t *testing.T) {
	in := workbookInput(t)
	asOf := in.AsOf
	in.Overrides = []NodeOverride{
		{NodeKey: "l1.metals::veto", AsOf: asOf.AddDays(-10), Value: datapoints.Bool(true), Reason: "gold froth"},
		{NodeKey: "debt.gilt::composite", AsOf: asOf, Value: datapoints.Num(80), Reason: "rate cut certain"},
		// Not active: expired, and starting later.
		{NodeKey: "l1.equity::composite", AsOf: asOf.AddDays(-30), ValidUntil: ptr(asOf.AddDays(-1)), Value: datapoints.Num(90), Reason: "old"},
		{NodeKey: "l1.debt::composite", AsOf: asOf.AddDays(1), Value: datapoints.Num(90), Reason: "future"},
		// Wrong type for the node: ignored.
		{NodeKey: "l1.debt::veto", AsOf: asOf, Value: datapoints.Num(1), Reason: "bad"},
	}
	res := eval(t, in)
	if n := res.Nodes["l1.metals::veto"]; !n.IsTrue() || !n.IsOverride || !strings.Contains(n.Formula, "gold froth") {
		t.Fatalf("metals veto = %+v", n)
	}
	if n := res.Nodes["debt.gilt::composite"]; num(n) != 80 || !n.IsOverride {
		t.Fatalf("gilt composite = %+v", n)
	}
	if got := num(res.Nodes["debt.gilt::tilt"]); math.Abs(got-0.18) > 1e-12 {
		t.Fatalf("overridden composite did not flow into tilt: %v", got)
	}
	for _, k := range []string{"l1.equity::composite", "l1.debt::composite", "l1.debt::veto"} {
		if res.Nodes[k].IsOverride {
			t.Errorf("%s overridden", k)
		}
	}
}

func TestSectorsHeldRules(t *testing.T) {
	cases := []struct {
		held   []string
		reason string
	}{
		{[]string{"sector.pharma"}, "not_qualifying"},
		{[]string{"sector.nope"}, "unknown_sector"},
		{[]string{"sector.capgoods", "sector.capgoods"}, "duplicate"},
	}
	for _, c := range cases {
		in := workbookInput(t)
		in.Params.SectorsHeld = c.held
		_, err := NewEngine().Evaluate(context.Background(), in)
		wantInvalid(t, err, c.reason)
	}

	// Three qualifying sectors, max 2.
	in := workbookInput(t)
	for _, sec := range []string{"sector.banking", "sector.pharma"} {
		for _, sg := range in.Config.SectorSatellite.Signals {
			in.Inputs[sec+"::"+sg.Key] = Input{Value: datapoints.Num(90)}
		}
	}
	in.Params.SectorsHeld = []string{"sector.banking", "sector.pharma", "sector.capgoods"}
	_, err := NewEngine().Evaluate(context.Background(), in)
	wantInvalid(t, err, "too_many")

	// A vetoed sector does not qualify whatever its score.
	in.Inputs["trig.flow_froth.sectoral"] = Input{Value: datapoints.Bool(true)}
	in.Params.SectorsHeld = []string{"sector.banking"}
	_, err = NewEngine().Evaluate(context.Background(), in)
	wantInvalid(t, err, "not_qualifying")

	// Two held: sleeve is the full cap, split equally.
	delete(in.Inputs, "trig.flow_froth.sectoral")
	in.Params.SectorsHeld = []string{"sector.banking", "sector.capgoods"}
	res := eval(t, in)
	if num(res.Nodes["l1.equity::sleeve"]) != 0.15 || num(res.Nodes["sector.banking::within"]) != 0.075 {
		t.Fatalf("sleeve %v", num(res.Nodes["l1.equity::sleeve"]))
	}

	// None held: the sleeve is empty and core equity keeps its weights.
	in.Params.SectorsHeld = nil
	res = eval(t, in)
	if num(res.Nodes["l1.equity::sleeve"]) != 0 || num(res.Nodes["equity.large::within"]) != num(res.Nodes["equity.large::weight"]) {
		t.Fatal("empty sleeve changed core equity")
	}
	for _, l := range res.Allocation {
		if l.Sleeve {
			t.Fatalf("sleeve line with no sectors held: %+v", l)
		}
	}
}

func TestSectorMaxTenure(t *testing.T) {
	in := workbookInput(t)
	in.History.QuartersHeld = map[string]int{"sector.capgoods": 3}
	eval(t, in)
	in.History.QuartersHeld["sector.capgoods"] = 4
	_, err := NewEngine().Evaluate(context.Background(), in)
	wantInvalid(t, err, "max_tenure")
}

func TestMaxMultipleOfNeutral(t *testing.T) {
	in := workbookInput(t)
	in.Config.Scorecards["l1"] = withCap(in.Config.Scorecards["l1"], 0.9)
	for _, sg := range in.Config.Scorecards["l1"].Signals {
		in.Inputs["l1.metals::"+sg.Key] = Input{Value: datapoints.Num(100)}
	}
	for i := range in.Config.Constraints {
		if in.Config.Constraints[i].Name == "max_multiple_of_neutral" {
			in.Config.Constraints[i].Params = json.RawMessage(`{"max": 1.5}`)
		}
	}
	res := eval(t, in)
	if n := res.Nodes["l1.metals::tilt"]; num(n) != 0.5 || !strings.Contains(n.Formula, "capped at 0.5") {
		t.Fatalf("metals tilt = %+v", n)
	}
	if num(res.Nodes["l1.debt::tilt"]) != tiltOf(57.5, 0.9, false) {
		t.Fatal("constraint touched another bucket")
	}
}

func withCap(sc Scorecard, k float64) Scorecard {
	sc.TiltCap = k
	return sc
}

func TestExtremeFullCap(t *testing.T) {
	in := workbookInput(t)
	for i := range in.Config.Constraints {
		if in.Config.Constraints[i].Name == "extreme_full_cap" {
			in.Config.Constraints[i].Params = json.RawMessage(`{"high": 60, "low": 20, "consecutive_reviews": 2, "normal_cap_factor": 0.5}`)
		}
	}
	// Gilt (64.25) is extreme now; with one earlier extreme review it gets
	// the full cap. Liquid (54.5) is not: half cap.
	in.History.ExtremeRuns = map[string]int{"debt.gilt": 1}
	res := eval(t, in)
	if !res.Nodes["debt.gilt::extreme"].IsTrue() || num(res.Nodes["debt.gilt::tilt"]) != tiltOf(64.25, 0.30, false) {
		t.Fatalf("gilt = %v", num(res.Nodes["debt.gilt::tilt"]))
	}
	if res.Nodes["debt.liquid::extreme"].IsTrue() || num(res.Nodes["debt.liquid::tilt"]) != tiltOf(54.5, 0.15, false) {
		t.Fatalf("liquid = %v", num(res.Nodes["debt.liquid::tilt"]))
	}
	// First extreme review only: still the normal cap.
	in.History.ExtremeRuns = nil
	res = eval(t, in)
	if res.Nodes["debt.gilt::extreme"].IsTrue() || num(res.Nodes["debt.gilt::tilt"]) != tiltOf(64.25, 0.15, false) {
		t.Fatalf("gilt first review = %v", num(res.Nodes["debt.gilt::tilt"]))
	}

	// The shipped factor 1.0 changes nothing.
	base := eval(t, workbookInput(t))
	for k, n := range base.Nodes {
		if strings.HasSuffix(k, "::tilt") && strings.Contains(n.Formula, "normal cap") {
			t.Fatalf("%s re-capped with factor 1", k)
		}
	}
}

func TestZeroSumNormalisation(t *testing.T) {
	in := workbookInput(t)
	in.Config.Normalisation = NormZeroSum
	res := eval(t, in)
	// f_b = n_b(1 + t_b − Σ n_j t_j); L1: Σ n t = 0.55×0.002 + 0.35×0.03 + 0.1×0.005 = 0.0121.
	if got := num(res.Nodes["l1.debt::weight"]); math.Abs(got-0.35*(1+0.03-0.0121)) > 1e-12 {
		t.Fatalf("debt = %v", got)
	}
	var sum float64
	for _, b := range []string{"l1.equity", "l1.debt", "l1.metals"} {
		sum += num(res.Nodes[b+"::weight"])
	}
	if math.Abs(sum-1) > 1e-12 {
		t.Fatalf("weights sum to %v", sum)
	}
}

func TestNeutralWeightsChecked(t *testing.T) {
	in := workbookInput(t)
	in.Profile.NeutralWeights["l1.debt"] = d("0.30")
	_, err := NewEngine().Evaluate(context.Background(), in)
	wantInvalid(t, err, "weights_sum")
	in = workbookInput(t)
	delete(in.Profile.NeutralWeights, "metals.silver")
	_, err = NewEngine().Evaluate(context.Background(), in)
	wantInvalid(t, err, "missing")
}

func TestEvaluateIsDeterministicAndPure(t *testing.T) {
	in := workbookInput(t)
	a := eval(t, in)
	for i := 0; i < 5; i++ {
		if b := eval(t, in); !reflect.DeepEqual(a, b) {
			t.Fatal("same input, different result")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewEngine().Evaluate(ctx, in); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled evaluate: %v", err)
	}
}

func TestStepOrderMatters(t *testing.T) {
	in := workbookInput(t)
	in.Config.Steps = []string{"tilt", "composite", "veto", "normalize", "rollup"}
	_, err := NewEngine().Evaluate(context.Background(), in)
	wantInvalid(t, err, "step_order")
}

func TestApportion(t *testing.T) {
	got, err := apportion([]float64{1.0 / 3, 1.0 / 3, 1.0 / 3}, "x")
	if err != nil {
		t.Fatal(err)
	}
	sum := decimal.Zero
	for _, g := range got {
		sum = sum.Add(g)
	}
	if !sum.Equal(decimal.NewFromInt(1)) || got[0].String() != "0.333334" || got[2].String() != "0.333333" {
		t.Fatalf("apportion = %v (sum %s)", got, sum)
	}
	if _, err := apportion([]float64{0.5, 0.4}, "x"); err == nil {
		t.Fatal("weights summing to 0.9 accepted")
	}
	if _, err := apportion([]float64{1.2, -0.2}, "x"); err == nil {
		t.Fatal("negative weight accepted")
	}
}

func TestCheckConfig(t *testing.T) {
	reg := NewEngine()
	if err := CheckConfig(seedConfig(t), reg); err != nil {
		t.Fatalf("shipped config: %v", err)
	}
	mutate := func(f func(c *Config)) Config {
		c := seedConfig(t)
		f(&c)
		return c
	}
	cases := []struct {
		name, path, reason string
		c                  Config
	}{
		{"unknown step", "/steps/8", "unknown_step", mutate(func(c *Config) { c.Steps = append(c.Steps, "optimise") })},
		{"weights", "/scorecards/debt/signals", "weights_sum", mutate(func(c *Config) {
			sc := c.Scorecards["debt"]
			sc.Signals = append([]Signal(nil), sc.Signals...)
			sc.Signals[0].Weight = 0.35
			c.Scorecards["debt"] = sc
		})},
		{"parent", "/scorecards/metals/parent", "unknown_parent", mutate(func(c *Config) {
			sc := c.Scorecards["metals"]
			sc.Parent = "l1.gold"
			c.Scorecards["metals"] = sc
		})},
		{"two roots", "/scorecards", "one_root", mutate(func(c *Config) {
			sc := c.Scorecards["metals"]
			sc.Parent = ""
			c.Scorecards["metals"] = sc
		})},
		{"duplicate bucket", "/sector_satellite/sectors/0", "duplicate_bucket", mutate(func(c *Config) {
			sat := *c.SectorSatellite
			sat.Sectors = append([]string{"equity.large"}, sat.Sectors...)
			c.SectorSatellite = &sat
		})},
		{"template", "/scorecards/l1/score_series", "template", mutate(func(c *Config) {
			sc := c.Scorecards["l1"]
			sc.ScoreSeries = "score.{bucket}"
			c.Scorecards["l1"] = sc
		})},
		{"veto key", "/scorecards/debt/vetoes/debt.nope", "unknown_bucket", mutate(func(c *Config) {
			sc := c.Scorecards["debt"]
			sc.Vetoes = map[string][]string{"debt.nope": {"trig.x"}}
			c.Scorecards["debt"] = sc
		})},
		{"constraint", "/constraints/3/name", "unknown_constraint", mutate(func(c *Config) {
			c.Constraints = append(c.Constraints, ConstraintRef{Name: "nope"})
		})},
		{"constraint params", "/constraints/0/params", "invalid_params", mutate(func(c *Config) {
			c.Constraints = append([]ConstraintRef(nil), c.Constraints...)
			c.Constraints[0].Params = json.RawMessage(`{"max": 0.5}`)
		})},
		{"constraint bucket", "/constraints/0/bucket", "unknown_bucket", mutate(func(c *Config) {
			c.Constraints = append([]ConstraintRef(nil), c.Constraints...)
			c.Constraints[0].Bucket = "metals.gold"
		})},
		{"normalisation", "/normalisation", "invalid_enum", mutate(func(c *Config) { c.Normalisation = "softmax" })},
	}
	for _, tc := range cases {
		err := CheckConfig(tc.c, reg)
		e, ok := apperr.As(err)
		if !ok || e.Code != apperr.ValidationFailed {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		found := false
		for _, det := range e.Details {
			if det.Path == tc.path && det.Reason == tc.reason {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: want %s %s, got %+v", tc.name, tc.path, tc.reason, e.Details)
		}
	}
}

type fakeLister map[string]bool

func (f fakeLister) ListSeries(_ context.Context, sf catalog.SeriesFilter) (paging.Page[catalog.Series], error) {
	var out []catalog.Series
	for _, k := range sf.Keys {
		if f[k] {
			out = append(out, catalog.Series{Key: k})
		}
	}
	return paging.Page[catalog.Series]{Items: out}, nil
}

func TestValidatorChecksSeries(t *testing.T) {
	c := seedConfig(t)
	refs := ReferencedSeries(c)
	// 3×5 + 4×5 + 3×4 + 2×3 + 8×4 scores; triggers: 2 L1, 4×3 + 1 equity,
	// 1 debt, 3 metals (trig.parabolic.gold is shared with L1), 8×2 + 1 sector.
	if want := 85 + 2 + 13 + 1 + 2 + 17; len(refs) != want {
		t.Fatalf("%d referenced series, want %d", len(refs), want)
	}
	all := fakeLister{}
	for k := range refs {
		all[k] = true
	}
	if err := NewValidator(NewEngine(), all).Validate(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	delete(all, "trig.currency_stress")
	delete(all, "metals.silver::industrial")
	err := NewValidator(NewEngine(), all).Validate(context.Background(), c)
	e, ok := apperr.As(err)
	if !ok || len(e.Details) != 2 {
		t.Fatalf("got %v", err)
	}
	want := []apperr.Detail{
		{Path: "/scorecards/metals/score_series", Reason: "unknown_series", Meta: map[string]any{"series": "metals.silver::industrial"}},
		{Path: "/scorecards/equity/vetoes/equity.intl/0", Reason: "unknown_series", Meta: map[string]any{"series": "trig.currency_stress"}},
	}
	if !reflect.DeepEqual(e.Details, want) {
		t.Fatalf("details = %+v", e.Details)
	}
}

func BenchmarkEvaluate(b *testing.B) {
	in := workbookInput(b)
	e := NewEngine()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := e.Evaluate(context.Background(), in); err != nil {
			b.Fatal(err)
		}
	}
}

func TestEvaluateLatency(t *testing.T) {
	// Plan target: preview p95 < 50 ms. One evaluation is far below it.
	in := workbookInput(t)
	e := NewEngine()
	var worst time.Duration
	for i := 0; i < 50; i++ {
		start := time.Now()
		if _, err := e.Evaluate(context.Background(), in); err != nil {
			t.Fatal(err)
		}
		worst = max(worst, time.Since(start))
	}
	if worst > 50*time.Millisecond {
		t.Fatalf("slowest evaluate took %s", worst)
	}
}
