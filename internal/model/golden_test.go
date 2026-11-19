package model

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"cloud.google.com/go/civil"
	"github.com/shopspring/decimal"
	"gopkg.in/yaml.v3"

	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/model/modeltest"
)

// The golden case (modeltest) is the MAAF v2 workbook as shared on 2026-11-19: its
// scoring-sheet inputs, its neutral weights, Small Cap vetoed, one sector
// (Capital Goods) held, and its cached outputs on every scoring sheet and
// the Portfolio Rollup.

// seedConfig loads the shipped model config.
func seedConfig(t testing.TB) Config {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "configs", "seed", "model_maaf_v1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Config any `yaml:"config"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	j, err := json.Marshal(doc.Config)
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := json.Unmarshal(j, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func workbookProfile() RiskProfile {
	w := map[string]decimal.Decimal{}
	for k, v := range modeltest.NeutralWeights {
		w[k] = d(v)
	}
	return RiskProfile{Key: "balanced", Version: 1, NeutralWeights: w}
}

var (
	workbookScores  = modeltest.Scores
	workbookOutputs = modeltest.Outputs
	workbookLines   = modeltest.Lines
)

func workbookInput(t testing.TB) EvalInput {
	c := seedConfig(t)
	in := EvalInput{
		AsOf: civil.Date{Year: 2026, Month: 3, Day: 31}, Config: c, Profile: workbookProfile(),
		Inputs: map[string]Input{}, Params: Params{SectorsHeld: modeltest.SectorsHeld},
	}
	signals := func(b string) []Signal {
		for _, sc := range c.Scorecards {
			if contains(sc.Buckets, b) {
				return sc.Signals
			}
		}
		return c.SectorSatellite.Signals
	}
	var id int64
	for b, vals := range workbookScores {
		for i, sg := range signals(b) {
			id++
			in.Inputs[b+"::"+sg.Key] = Input{Value: datapoints.Num(vals[i]), DatapointID: id, Source: "analyst"}
		}
	}
	// Equity Segments D11: Small Cap vetoed.
	in.Inputs[modeltest.VetoTrigger] = Input{Value: datapoints.Bool(true), DatapointID: 999, Source: "computed"}
	return in
}

func TestGoldenWorkbook(t *testing.T) {
	res, err := NewEngine().Evaluate(context.Background(), workbookInput(t))
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range workbookOutputs {
		n, ok := res.Nodes[key]
		if !ok || n.Num == nil {
			t.Errorf("%s missing", key)
			continue
		}
		// The sheet's cached values carry 10 significant digits.
		if math.Abs(*n.Num-want) > 1e-9 {
			t.Errorf("%s = %.12g, workbook %.12g", key, *n.Num, want)
		}
	}
	for _, b := range []string{"equity.small"} {
		if !res.Nodes[b+"::veto"].IsTrue() {
			t.Errorf("%s not vetoed", b)
		}
	}
	if !res.Nodes["sector.capgoods::qualifies"].IsTrue() || res.Nodes["sector.pharma::qualifies"].IsTrue() {
		t.Error("only Capital Goods qualifies on the sheet")
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings: %+v", res.Warnings)
	}

	// The stored allocation: within 0.05pp of the sheet, exact sums.
	if len(res.Allocation) != len(workbookLines) {
		t.Fatalf("%d lines, want %d", len(res.Allocation), len(workbookLines))
	}
	total := decimal.Zero
	groups := map[string]decimal.Decimal{}
	for i, l := range res.Allocation {
		w := workbookLines[i]
		if l.LineItem != w.Item || l.Group != w.Group {
			t.Fatalf("line %d = %s/%s, want %s/%s", i, l.LineItem, l.Group, w.Item, w.Group)
		}
		if diff := math.Abs(l.PortfolioWeight.InexactFloat64() - w.Weight); diff > 0.0005 {
			t.Errorf("%s = %s, workbook %.10f (off %.4fpp)", w.Item, l.PortfolioWeight, w.Weight, diff*100)
		}
		if l.PortfolioWeight.Exponent() < -weightPlaces {
			t.Errorf("%s has more than %d decimals: %s", w.Item, weightPlaces, l.PortfolioWeight)
		}
		total = total.Add(l.PortfolioWeight)
		groups[l.Group] = groups[l.Group].Add(l.WithinGroup)
	}
	if !total.Equal(decimal.NewFromInt(1)) {
		t.Errorf("portfolio sums to %s", total)
	}
	for g, sum := range groups {
		if !sum.Equal(decimal.NewFromInt(1)) {
			t.Errorf("group %s sums to %s", g, sum)
		}
	}
	if l := res.Allocation[4]; !l.Sleeve || l.NeutralWeight.Sign() != 0 {
		t.Errorf("sector line = %+v", l)
	}
	if l := res.Allocation[2]; !l.Veto || !l.NeutralWeight.Equal(d("0.0825")) {
		t.Errorf("small cap line = %+v", l)
	}
}

// IsTrue reports a true bool node (test helper).
func (n Node) IsTrue() bool { return n.Bool != nil && *n.Bool }
