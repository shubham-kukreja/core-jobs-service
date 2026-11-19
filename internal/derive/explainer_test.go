package derive

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/civil"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	dpmocks "github.com/shubham-kukreja/core-jobs-service/internal/datapoints/mocks"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
)

// memStore is a point-in-time store: rows are visible when known_at <= cutoff.
type memStore map[string][]datapoints.Resolved

func (m memStore) add(key string, d civil.Date, v float64, known time.Time, ver int) {
	m[key] = append(m[key], datapoints.Resolved{
		ID: int64(len(m[key]) + 1), SeriesKey: key, ObsDate: d, Value: datapoints.Num(v), Source: "file_import", KnownAt: known, DerivVer: ver,
	})
	sort.Slice(m[key], func(i, j int) bool { return m[key][i].ObsDate.Before(m[key][j].ObsDate) })
}

func (m memStore) reader() *dpmocks.Reader {
	rng := func(key string, from, to civil.Date, o datapoints.ReadOpts) []datapoints.Resolved {
		var out []datapoints.Resolved
		for _, r := range m[key] {
			if !r.ObsDate.Before(from) && !r.ObsDate.After(to) && !r.KnownAt.After(o.Cutoff) {
				out = append(out, r)
			}
		}
		return out
	}
	return &dpmocks.Reader{
		AtFunc: func(_ context.Context, key string, d civil.Date, o datapoints.ReadOpts) (*datapoints.Resolved, error) {
			if rows := rng(key, d, d, o); len(rows) > 0 {
				return &rows[0], nil
			}
			return nil, nil
		},
		RangeFunc: func(_ context.Context, key string, from, to civil.Date, o datapoints.ReadOpts) ([]datapoints.Resolved, error) {
			return rng(key, from, to, o), nil
		},
		HistoryFunc: func(_ context.Context, key string, upTo civil.Date, w datapoints.Months, o datapoints.ReadOpts) ([]datapoints.Resolved, error) {
			return rng(key, calendarStartAfter(upTo, int(w)), upTo, o), nil
		},
	}
}

func calendarStartAfter(upTo civil.Date, months int) civil.Date {
	return monthsBack(upTo, months).AddDays(1)
}

type memCatalog map[string]catalog.Series

func (c memCatalog) Series(_ context.Context, key string) (catalog.Series, error) {
	s, ok := c[key]
	if !ok {
		return s, catalog.ErrNotFound
	}
	return s, nil
}

func derived(key, fn string, in map[string]string, params string) catalog.Series {
	return catalog.Series{Key: key, Name: key, Derivation: &catalog.Derivation{Fn: fn, Inputs: in, Params: json.RawMessage(params), Version: 1}}
}

// valuationFixture is the plan's Equity - Valuation shape on four months:
// score = (gap + (100 − pe)) / 2 over two percentiles, one of an
// earnings-yield gap derived from P/E and the 10Y yield.
//
//	P/E 20, 22, 18, 21; 10Y 7 each month.
//	EY gap = 100/PE − 7: −2, −2.4545, −1.4444, −2.2381
//	Apr: pctile(PE 21) = 100 × 2/3 = 66.67; pctile(gap −2.2381) = 100 × 1/3 = 33.33
//	score = (33.33 + (100 − 66.67)) / 2 = 33.33
func valuationFixture() (memCatalog, memStore, time.Time) {
	cat := memCatalog{
		"pe.nifty50":    {Key: "pe.nifty50", Name: "Nifty 50 trailing P/E"},
		"yield.gsec10y": {Key: "yield.gsec10y", Name: "10Y G-sec yield", Unit: "%"},
		"gap.ey":        derived("gap.ey", "expr", map[string]string{"pe": "pe.nifty50", "y": "yield.gsec10y"}, `{"expr":"100 / pe - y"}`),
		"pctile.gap":    derived("pctile.gap", "percentile_rank", map[string]string{"x": "gap.ey"}, `{"window_months":12}`),
		"pctile.pe":     derived("pctile.pe", "percentile_rank", map[string]string{"x": "pe.nifty50"}, `{"window_months":12}`),
		"val": derived("val", "expr", map[string]string{"gap": "pctile.gap", "pe": "pctile.pe"},
			`{"expr":"(gap + (100 - pe)) / 2"}`),
	}
	v := cat["val"]
	v.Registry.DisplayName = "Equity - Valuation"
	cat["val"] = v
	known := time.Date(2025, 5, 10, 0, 0, 0, 0, time.UTC)
	st := memStore{}
	pes := []float64{20, 22, 18, 21}
	for i, pe := range pes {
		d := me(2025, i+1)
		st.add("pe.nifty50", d, pe, known, 0)
		st.add("yield.gsec10y", d, 7, known, 0)
		st.add("gap.ey", d, 100/pe-7, known, 1)
	}
	st.add("pctile.pe", me(2025, 4), 200.0/3, known, 1)
	st.add("pctile.gap", me(2025, 4), 100.0/3, known, 1)
	st.add("val", me(2025, 4), 100.0/3, known, 1)
	// A May reading published after the cutoff used below.
	st.add("pe.nifty50", me(2025, 5), 30, known.AddDate(0, 1, 0), 0)
	st.add("val", me(2025, 5), 99, known.AddDate(0, 1, 0), 1)
	return cat, st, known
}

func newTestExplainer(cat memCatalog, st memStore, now time.Time) Explainer {
	return NewExplainer(ExplainerDeps{
		Catalog: cat, Reader: st.reader(), Registry: NewDefaultRegistry(), Clock: clock.NewFake(now),
		Governance: Governance{PercentileMinObservations: 3},
	})
}

func TestExplainValuationTree(t *testing.T) {
	cat, st, known := valuationFixture()
	ex, err := newTestExplainer(cat, st, known.Add(time.Hour)).Explain(context.Background(), "val", civil.Date{}, datapoints.ReadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	// Latest at the cutoff is April: May is not yet known.
	if ex.ObsDate != me(2025, 4) || ex.Point == nil || math.Abs(*ex.Point.Value.Num-100.0/3) > 1e-9 {
		t.Fatalf("top = %s %+v", ex.ObsDate, ex.Point)
	}
	if ex.Formula != "(gap + (100 - pe)) / 2" {
		t.Fatalf("formula = %q", ex.Formula)
	}
	if got := ex.Steps[len(ex.Steps)-1].Text; got != "(gap + (100 - pe)) / 2 = (33.33 + (100 - 66.67)) / 2 = 33.33" {
		t.Fatalf("final = %q", got)
	}
	if len(ex.Parts) != 2 || ex.Parts[0].Alias != "gap" || ex.Parts[1].Alias != "pe" {
		t.Fatalf("parts = %+v", ex.Parts)
	}
	pe := ex.Parts[1]
	if len(pe.Parts) != 0 || len(pe.Inputs) != 1 || pe.Inputs[0].Expanded || pe.Inputs[0].Point == nil || *pe.Inputs[0].Point.Value.Num != 21 {
		t.Fatalf("pe part = %+v", pe)
	}
	if want := []string{
		"Window: 4 readings from 2024-05-01 to 2025-04-30 (12 months; at least 3 needed)",
		"PERCENTRANK(4 readings, 21) × 100 = 66.67",
	}; texts(pe.Steps) != strings.Join(want, "|") {
		t.Fatalf("pe steps = %s", texts(pe.Steps))
	}
	gap := ex.Parts[0]
	if len(gap.Parts) != 1 || gap.Parts[0].Series.Key != "gap.ey" {
		t.Fatalf("gap part = %+v", gap)
	}
	if got := texts(gap.Parts[0].Steps); got != "100 / pe - y = 100 / 21 - 7 = -2.24" {
		t.Fatalf("gap.ey steps = %s", got)
	}
	// Stored values match today's formula everywhere: no drift notes.
	walk(ex, func(e Explanation) {
		if len(e.Notes) > 0 {
			t.Errorf("%s notes: %v", e.Series.Key, e.Notes)
		}
	})
}

func TestExplainNotesDriftAndMissing(t *testing.T) {
	cat, st, known := valuationFixture()
	st["val"][0].Value = datapoints.Num(50) // stored with an older formula
	st["val"][0].DerivVer = 1
	v := cat["val"]
	v.Derivation.Version = 2
	cat["val"] = v
	e := newTestExplainer(cat, st, known.Add(time.Hour))
	ex, err := e.Explain(context.Background(), "val", me(2025, 4), datapoints.ReadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Notes) != 1 || !strings.Contains(ex.Notes[0], "Stored value 50 was computed with derivation v1; the current formula (v2) gives 33.33") {
		t.Fatalf("notes = %v", ex.Notes)
	}

	// A date with inputs but no stored score.
	delete(st, "pctile.pe")
	ex, err = e.Explain(context.Background(), "pctile.gap", me(2025, 3), datapoints.ReadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if ex.Point != nil || len(ex.Notes) != 1 || !strings.HasPrefix(ex.Notes[0], "No stored value at 2025-03-31") {
		t.Fatalf("missing = %+v", ex)
	}

	// No value at all at the cutoff.
	ex, err = e.Explain(context.Background(), "pctile.pe", civil.Date{}, datapoints.ReadOpts{})
	if err != nil || !ex.ObsDate.IsZero() || ex.Point != nil || ex.Formula == "" {
		t.Fatalf("empty = %+v, %v", ex, err)
	}
	if _, err := e.Explain(context.Background(), "nope", civil.Date{}, datapoints.ReadOpts{}); err == nil {
		t.Fatal("unknown series explained")
	}
}

func TestExplainIsBounded(t *testing.T) {
	// A chain longer than maxExplainDepth stops expanding.
	cat := memCatalog{"s0": {Key: "s0"}}
	st := memStore{}
	d := me(2025, 1)
	st.add("s0", d, 1, time.Time{}, 0)
	for i := 1; i <= 20; i++ {
		k, prev := "s"+strconv.Itoa(i), "s"+strconv.Itoa(i-1)
		cat[k] = derived(k, "invert", map[string]string{"x": prev}, `{}`)
		st.add(k, d, float64(100*(i%2)+(1-2*(i%2))*1), time.Time{}, 1)
	}
	ex, err := newTestExplainer(cat, st, time.Now()).Explain(context.Background(), "s20", d, datapoints.ReadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	depth := 0
	for e := ex; len(e.Parts) > 0; e = e.Parts[0] {
		depth++
	}
	if depth != maxExplainDepth-1 {
		t.Fatalf("expanded %d levels, want %d", depth, maxExplainDepth-1)
	}
}

func TestFormula(t *testing.T) {
	cases := []struct {
		der  *catalog.Derivation
		want string
	}{
		{nil, ""},
		{&catalog.Derivation{Fn: "expr", Params: json.RawMessage(`{"expr":"a + b"}`)}, "a + b"},
		{&catalog.Derivation{Fn: "percentile_rank", Inputs: map[string]string{"x": "pe"}, Params: json.RawMessage(`{"window_months":180}`)},
			"percentile_rank(x = pe), window_months 180"},
		{&catalog.Derivation{Fn: "avg", Inputs: map[string]string{"b": "y", "a": "x"}}, "avg(a = x, b = y)"},
	}
	for _, c := range cases {
		if got := formula(c.der); got != c.want {
			t.Errorf("formula = %q, want %q", got, c.want)
		}
	}
}

func TestExplainStepTexts(t *testing.T) {
	d := me(2026, 3)
	cases := []struct {
		fn, params string
		in         Inputs
		want       string
	}{
		{"rolling_mean", `{"periods":3}`, Inputs{"x": monthly(2026, 1, 1, 2, 6)}, "(1 + 2 + 6) / 3 = 3"},
		{"ratio_of", `{"scale":100}`, Inputs{"a": monthly(2026, 3, 5), "b": monthly(2026, 3, 4)}, "100 × a ÷ b = 100 × 5 ÷ 4 = 125"},
		{"ratio_of", `{}`, Inputs{"a": monthly(2026, 3, 5), "b": monthly(2026, 3, 4)}, "a ÷ b = 5 ÷ 4 = 1.25"},
		{"pct_change", `{"months":2}`, Inputs{"x": monthly(2026, 1, 50, 0, 60)}, "100 × (x(2026-03-31) ÷ x(2026-01-31) − 1) = 100 × (60 ÷ 50 − 1) = 20"},
		{"diff_over", `{"months":1}`, Inputs{"x": monthly(2026, 2, 7, 6.5)}, "x(2026-03-31) − x(2026-02-28) = 6.5 − 7 = -0.5"},
		{"invert", `{}`, Inputs{"x": monthly(2026, 3, 30)}, "100 − 30 = 70"},
		{"avg", `{}`, Inputs{"a": monthly(2026, 3, 10), "b": monthly(2026, 3, 20), "c": nil}, "(10 + 20) / 2 = 15"},
		{"rubric", `{"base":50,"clamp":[0,100],"add":{"s":{"cutting":20},"g":{"miss":-10}}}`,
			Inputs{"s": cat(d, "cutting"), "g": cat(d, "miss")}, "clamp(50 − 10 + 20, 0, 100) = 60"},
		{"bucket_map", `{"ranges":[{"max":0,"score":10},{"min":0,"score":90}]}`, Inputs{"x": monthly(2026, 3, 2)}, "x = 2 falls in [0, ∞) = 90"},
		{"const", `{"value":5}`, Inputs{}, "Constant = 5"},
		{"threshold", `{"op":">","value":3}`, Inputs{"x": monthly(2026, 3, 4)}, "x > 3, with x = 4 = true"},
		{"consecutive", `{"op":"<","value":0,"periods":2}`, Inputs{"x": monthly(2026, 1, 5, -1, -2)}, "x < 0 on each of the last 2 observations [-1, -2] = true"},
		{"fallback", `{"order":["a","b"]}`, Inputs{"a": nil, "b": monthly(2026, 3, 8)}, "first available: b = 8 = 8"},
		{"fallback", `{"order":["a"],"default":50}`, Inputs{"a": nil}, "no input has a value; default = 50"},
		{"invert", `{}`, Inputs{"x": nil}, "100 − missing → no value"},
	}
	for _, c := range cases {
		p, _ := NewDefaultRegistry().Get(c.fn)
		steps, err := p.Explain(context.Background(), Env{Governance: Governance{PercentileMinObservations: 3}}, d, c.in, json.RawMessage(c.params))
		if err != nil {
			t.Fatalf("%s: %v", c.fn, err)
		}
		if got := steps[len(steps)-1].Text; got != c.want {
			t.Errorf("%s: got %q, want %q", c.fn, got, c.want)
		}
	}
}

func texts(s []ExplainStep) string {
	out := make([]string, len(s))
	for i, st := range s {
		out[i] = st.Text
	}
	return strings.Join(out, "|")
}

func walk(e Explanation, f func(Explanation)) {
	f(e)
	for _, p := range e.Parts {
		walk(p, f)
	}
}
