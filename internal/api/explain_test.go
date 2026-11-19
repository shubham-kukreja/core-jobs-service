package api

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"cloud.google.com/go/civil"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/derive"
)

// valuationExplanation is the plan's Equity - Valuation breakdown as the
// explainer returns it: (gap + (100 − pe)) / 2 over two percentiles.
func valuationExplanation() derive.Explanation {
	d := civil.Date{Year: 2026, Month: 10, Day: 9}
	jul := civil.Date{Year: 2026, Month: 7, Day: 1}
	f := func(v float64) *float64 { return &v }
	pt := func(key string, d civil.Date, v float64, src string) *datapoints.Resolved {
		return &datapoints.Resolved{SeriesKey: key, ObsDate: d, Value: datapoints.Num(v), Source: src, DerivVer: 1}
	}
	pe := catalog.Series{Key: "pe.nifty50", Name: "Nifty 50 trailing P/E"}
	y := catalog.Series{Key: "yield.gsec10y", Name: "10Y G-sec yield", Unit: "%"}
	gapEY := catalog.Series{Key: "gap.earnings_yield", Name: "Earnings-yield gap", Derivation: &catalog.Derivation{Fn: "expr"}}
	pGap := catalog.Series{Key: "pctile.180m.gap.earnings_yield", Name: "Earnings-yield gap percentile", Derivation: &catalog.Derivation{Fn: "percentile_rank"}}
	pPE := catalog.Series{Key: "pctile.180m.pe.nifty50", Name: "P/E percentile", Derivation: &catalog.Derivation{Fn: "percentile_rank"}}
	top := catalog.Series{Key: "l1.equity::valuation", Name: "Equity - Valuation", Frequency: catalog.FreqMonthly,
		Registry: catalog.RegistryMeta{DisplayName: "Equity - Valuation", StaleAfter: 40 * 24 * time.Hour}, Derivation: &catalog.Derivation{Fn: "expr"}}
	return derive.Explanation{
		Series: top, ObsDate: d, Point: pt(top.Key, d, 84.9, "computed"), Formula: "(gap + (100 - pe)) / 2",
		Inputs: []derive.ExplainInput{
			{Alias: "gap", Series: pGap, Point: pt(pGap.Key, d, 70.3, "computed"), Expanded: true},
			{Alias: "pe", Series: pPE, Point: pt(pPE.Key, d, 0.53, "computed"), Expanded: true},
		},
		Steps: []derive.ExplainStep{{Text: "(gap + (100 - pe)) / 2 = (70.3 + (100 - 0.53)) / 2 = 84.9", Result: f(84.9)}},
		Parts: []derive.Explanation{
			{
				Series: pGap, Alias: "gap", ObsDate: d, Point: pt(pGap.Key, d, 70.3, "computed"),
				Inputs: []derive.ExplainInput{{Alias: "x", Series: gapEY, Point: pt(gapEY.Key, d, -1.97, "computed"), Expanded: true}},
				Steps: []derive.ExplainStep{
					{Text: "Window: 708 readings from 2011-10-10 to 2026-10-09 (180 months; at least 24 needed)"},
					{Text: "PERCENTRANK(708 readings, -1.97) × 100 = 70.3", Result: f(70.3)},
				},
				Parts: []derive.Explanation{{
					Series: gapEY, Alias: "x", ObsDate: d, Point: pt(gapEY.Key, d, -1.97, "computed"),
					Inputs: []derive.ExplainInput{
						{Alias: "pe", Series: pe, Point: pt(pe.Key, d, 20.78, "nse")},
						{Alias: "y", Series: y, Point: pt(y.Key, jul, 6.78, "fred")},
					},
					Steps: []derive.ExplainStep{{Text: "100 / pe - y = 100 / 20.78 - 6.78 = -1.97", Result: f(-1.97)}},
					Notes: []string{"Stored value -1.9 was computed with derivation v1; the current formula (v2) gives -1.97."},
				}},
			},
			{
				Series: pPE, Alias: "pe", ObsDate: d, Point: pt(pPE.Key, d, 0.53, "computed"),
				Inputs: []derive.ExplainInput{{Alias: "x", Series: pe, Point: pt(pe.Key, d, 20.78, "nse")}},
				Steps:  []derive.ExplainStep{{Text: "PERCENTRANK(755 readings, 20.78) × 100 = 0.53", Result: f(0.53)}},
			},
		},
	}
}

func TestExplainSeries(t *testing.T) {
	h := newHarness(t)
	h.cat.ListSourcesFunc = func(context.Context) ([]catalog.Source, error) {
		return []catalog.Source{{Key: "nse", Name: "NSE"}, {Key: "fred", Name: "FRED"}}, nil
	}
	var gotDate civil.Date
	var gotCut time.Time
	h.explain.ExplainFunc = func(_ context.Context, key string, d civil.Date, o datapoints.ReadOpts) (derive.Explanation, error) {
		if key != "l1.equity::valuation" {
			return derive.Explanation{}, catalog.ErrNotFound
		}
		gotDate, gotCut = d, o.Cutoff
		return valuationExplanation(), nil
	}

	rec := h.do(req{method: "GET", path: "/v1/series/l1.equity::valuation/explain", op: "/v1/series/{key}/explain", token: viewerToken})
	wantStatus(t, rec, 200)
	if !gotDate.IsZero() || !gotCut.Equal(testNow) {
		t.Fatalf("explain called with %s, %s", gotDate, gotCut)
	}
	var got explanationJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Score == nil || *got.Score != 84.9 || *got.Observed != "2026-10-09" || got.Freshness != "fresh" {
		t.Fatalf("header = %+v", got)
	}
	if got.Final != "(gap + (100 - pe)) / 2 = (70.3 + (100 - 0.53)) / 2 = 84.9" || got.Formula != "(gap + (100 - pe)) / 2" {
		t.Fatalf("final = %q, formula = %q", got.Final, got.Formula)
	}
	// Leaves only, deduplicated: the P/E appears under both parts.
	if !reflect.DeepEqual(got.Sources, []string{"NSE", "FRED"}) || len(got.Inputs) != 2 ||
		got.Inputs[0] != (explainInputJSON{Key: "pe.nifty50", Label: "Nifty 50 trailing P/E", Value: 20.78, Source: "NSE", Observed: "2026-10-09"}) ||
		got.Inputs[1] != (explainInputJSON{Key: "yield.gsec10y", Label: "10Y G-sec yield", Value: 6.78, Unit: "%", Source: "FRED", Observed: "2026-07-01"}) {
		t.Fatalf("sources %v inputs %+v", got.Sources, got.Inputs)
	}
	wantParts := []explainPartJSON{
		{ID: "gap", Title: "Earnings-yield gap percentile", Result: ptr(70.3), Direction: "as_is", Steps: []string{
			"Earnings-yield gap: 100 / pe - y = 100 / 20.78 - 6.78 = -1.97",
			"Window: 708 readings from 2011-10-10 to 2026-10-09 (180 months; at least 24 needed)",
			"PERCENTRANK(708 readings, -1.97) × 100 = 70.3",
		}},
		{ID: "pe", Title: "P/E percentile", Result: ptr(0.53), Direction: "as_is", Steps: []string{"PERCENTRANK(755 readings, 20.78) × 100 = 0.53"}},
	}
	if !reflect.DeepEqual(got.Parts, wantParts) {
		t.Fatalf("parts = %+v", got.Parts)
	}
	if !reflect.DeepEqual(got.Notes, []string{"Earnings-yield gap: Stored value -1.9 was computed with derivation v1; the current formula (v2) gives -1.97."}) {
		t.Fatalf("notes = %v", got.Notes)
	}

	// obs_date and cutoff pass through; a late cutoff makes the value stale.
	h.clock.Set(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	rec = h.do(req{method: "GET", path: "/v1/series/l1.equity::valuation/explain?obs_date=2026-10-09&cutoff=2026-12-31T00:00:00Z", op: "/v1/series/{key}/explain", token: viewerToken})
	wantStatus(t, rec, 200)
	if gotDate != (civil.Date{Year: 2026, Month: 10, Day: 9}) || !gotCut.Equal(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("explain called with %s, %s", gotDate, gotCut)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Freshness != "stale" {
		t.Fatalf("freshness = %q, %v", got.Freshness, err)
	}

	wantCode(t, h.do(req{method: "GET", path: "/v1/series/l1.equity::valuation/explain?obs_date=2026-13-01", op: "/v1/series/{key}/explain", token: viewerToken}), 400, "validation_failed")
	wantCode(t, h.do(req{method: "GET", path: "/v1/series/nope/explain", op: "/v1/series/{key}/explain", token: viewerToken}), 404, "not_found")
	wantStatus(t, h.do(req{method: "GET", path: "/v1/series/l1.equity::valuation/explain"}), 401)
}

func TestExplainRawAndMissing(t *testing.T) {
	h := newHarness(t)
	h.cat.ListSourcesFunc = func(context.Context) ([]catalog.Source, error) { return nil, nil }
	s := testSeries()
	h.explain.ExplainFunc = func(_ context.Context, _ string, d civil.Date, _ datapoints.ReadOpts) (derive.Explanation, error) {
		if d.IsZero() {
			return derive.Explanation{Series: s}, nil
		}
		return derive.Explanation{Series: s, ObsDate: d, Point: &datapoints.Resolved{ObsDate: d, Value: num(21.5), Source: "analyst"}}, nil
	}
	rec := h.do(req{method: "GET", path: "/v1/series/pe.nifty100/explain", op: "/v1/series/{key}/explain", token: viewerToken})
	wantStatus(t, rec, 200)
	var got explanationJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Freshness != "missing" || got.Score != nil || got.Observed != nil || got.Formula != "Recorded value (no formula)" {
		t.Fatalf("missing = %+v", got)
	}
	rec = h.do(req{method: "GET", path: "/v1/series/pe.nifty100/explain?obs_date=2026-10-31", op: "/v1/series/{key}/explain", token: viewerToken})
	wantStatus(t, rec, 200)
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if *got.Score != 21.5 || !reflect.DeepEqual(got.Sources, []string{"analyst"}) || len(got.Inputs) != 0 {
		t.Fatalf("raw = %+v", got)
	}
}
