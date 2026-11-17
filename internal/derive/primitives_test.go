package derive

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"cloud.google.com/go/civil"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/calendar"
)

func me(y, m int) civil.Date {
	return calendar.MonthEnd(civil.Date{Year: y, Month: time.Month(m), Day: 1})
}

// monthly builds a month-end history starting at (y, m).
func monthly(y, m int, vals ...float64) []datapoints.Resolved {
	out := make([]datapoints.Resolved, len(vals))
	d := me(y, m)
	for i, v := range vals {
		out[i] = datapoints.Resolved{ObsDate: d, Value: datapoints.Num(v)}
		d = calendar.MonthEnd(calendar.AddMonths(d, 1))
	}
	return out
}

func cat(d civil.Date, s string) []datapoints.Resolved {
	return []datapoints.Resolved{{ObsDate: d, Value: datapoints.Cat(s)}}
}

func run(t *testing.T, name string, d civil.Date, in Inputs, params string) datapoints.Value {
	t.Helper()
	p, ok := NewDefaultRegistry().Get(name)
	if !ok {
		t.Fatalf("no primitive %s", name)
	}
	raw := json.RawMessage(params)
	if err := p.ValidateParams(raw); err != nil {
		t.Fatalf("%s params: %v", name, err)
	}
	env := Env{Governance: Governance{PercentileMinObservations: 3}}
	v, err := p.Compute(context.Background(), env, d, in, raw)
	if err != nil {
		t.Fatalf("%s compute: %v", name, err)
	}
	if _, err := p.Explain(context.Background(), env, d, in, raw); err != nil {
		t.Fatalf("%s explain: %v", name, err)
	}
	return v
}

func wantNum(t *testing.T, v datapoints.Value, want float64) {
	t.Helper()
	if v.Num == nil || math.Abs(*v.Num-want) > 1e-9 {
		t.Fatalf("got %s, want %v", fmtVal(v), want)
	}
}

func wantBool(t *testing.T, v datapoints.Value, want bool) {
	t.Helper()
	if v.Bool == nil || *v.Bool != want {
		t.Fatalf("got %s, want %v", fmtVal(v), want)
	}
}

func wantNull(t *testing.T, v datapoints.Value) {
	t.Helper()
	if !v.IsNull() {
		t.Fatalf("got %s, want null", fmtVal(v))
	}
}

func TestExpr(t *testing.T) {
	d := me(2026, 1)
	in := Inputs{"pe": monthly(2026, 1, 20), "y": monthly(2026, 1, 3)}
	wantNum(t, run(t, "expr", d, in, `{"expr":"100 / pe - y"}`), 2)
	wantBool(t, run(t, "expr", d, in, `{"expr":"pe > 10"}`), true)
	wantNull(t, run(t, "expr", me(2026, 2), in, `{"expr":"100 / pe - y"}`))
	wantNull(t, run(t, "expr", d, Inputs{"a": monthly(2026, 1, 1), "b": monthly(2026, 1, 0)}, `{"expr":"a / b"}`))
}

func TestRollingMean(t *testing.T) {
	in := Inputs{"x": monthly(2025, 10, 1, 2, 3, 4)}
	wantNum(t, run(t, "rolling_mean", me(2026, 1), in, `{"periods":3}`), 3)
	wantNull(t, run(t, "rolling_mean", me(2025, 11), in, `{"periods":3}`))
}

func TestRatioOf(t *testing.T) {
	in := Inputs{"a": monthly(2026, 3, 5), "b": monthly(2026, 1, 200)}
	wantNull(t, run(t, "ratio_of", me(2026, 3), in, `{"scale":100}`))
	wantNum(t, run(t, "ratio_of", me(2026, 3), in, `{"scale":100,"max_lag_months":3}`), 2.5)
	wantNull(t, run(t, "ratio_of", me(2026, 3), in, `{"max_lag_months":1}`))
}

func TestPctChangeAndDiff(t *testing.T) {
	in := Inputs{"x": monthly(2025, 2, append([]float64{100}, make([]float64, 11)...)...)}
	in["x"] = append(in["x"][:1], monthly(2026, 2, 140)...) // Feb-2025 100, Feb-2026 140
	wantNum(t, run(t, "pct_change", me(2026, 2), in, `{"months":12}`), 40)
	wantNum(t, run(t, "diff_over", me(2026, 2), in, `{"months":12}`), 40)
	wantNull(t, run(t, "pct_change", me(2026, 2), in, `{"months":6}`))
}

// The plan's bug: Jan-2026 ranked 100 because Feb–Mar were in the window.
func TestPercentileRankNeverReadsFuture(t *testing.T) {
	in := Inputs{"x": monthly(2025, 10, 1, 2, 3, 4, 10, 20)} // Oct..Mar
	v := run(t, "percentile_rank", me(2026, 1), in, `{"window_months":180,"min_obs":3}`)
	wantNum(t, v, 100) // 4 is the max of Oct..Jan only; Feb/Mar must not count
	in = Inputs{"x": monthly(2025, 10, 1, 2, 3, 2.5, 10, 20)}
	wantNum(t, run(t, "percentile_rank", me(2026, 1), in, `{"window_months":180,"min_obs":3}`), 200.0/3)
}

func TestPercentileRankMinObsAndWindow(t *testing.T) {
	in := Inputs{"x": monthly(2025, 12, 1, 2)}
	wantNull(t, run(t, "percentile_rank", me(2026, 1), in, `{"window_months":180,"min_obs":3}`))
	wantNull(t, run(t, "percentile_rank", me(2026, 1), in, `{"window_months":180}`)) // governance min 3
	in = Inputs{"x": monthly(2025, 1, 9, 9, 9, 1, 2, 3)}                             // Jan..Jun
	wantNum(t, run(t, "percentile_rank", me(2025, 6), in, `{"window_months":3,"min_obs":2}`), 100)
}

func TestInvertAvgConst(t *testing.T) {
	d := me(2026, 1)
	wantNum(t, run(t, "invert", d, Inputs{"x": monthly(2026, 1, 30)}, `{}`), 70)
	wantNum(t, run(t, "avg", d, Inputs{"a": monthly(2026, 1, 40), "b": monthly(2026, 1, 60), "c": nil}, `{}`), 50)
	wantNull(t, run(t, "avg", d, Inputs{"a": nil}, `{}`))
	wantNum(t, run(t, "const", d, nil, `{"value":90}`), 90)
}

func TestRubric(t *testing.T) {
	d := me(2026, 1)
	params := `{"base":50,"clamp":[0,100],"add":{
		"stance":{"cutting":20,"hold":0,"hiking":-20},
		"growth":{"beat":10,"in_line":0,"miss":-10},
		"infl":{"falling":5,"stable":0,"rising":-5}}}`
	in := Inputs{"stance": cat(d, "cutting"), "growth": cat(d, "beat"), "infl": cat(d, "falling")}
	wantNum(t, run(t, "rubric", d, in, params), 85)
	in["infl"] = nil
	wantNull(t, run(t, "rubric", d, in, params))

	p, _ := NewDefaultRegistry().Get("rubric")
	in["infl"] = cat(d, "unknown")
	if _, err := p.Compute(context.Background(), Env{}, d, in, json.RawMessage(params)); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("unknown category: got %v", err)
	}
	wantNum(t, run(t, "rubric", d, Inputs{"a": cat(d, "x")}, `{"base":90,"clamp":[0,100],"add":{"a":{"x":30}}}`), 100)
}

func TestBucketMap(t *testing.T) {
	d := me(2026, 1)
	ranges := `{"ranges":[{"max":2,"score":80},{"min":2,"max":5,"score":60},{"min":5,"score":40}]}`
	wantNum(t, run(t, "bucket_map", d, Inputs{"x": monthly(2026, 1, 1.5)}, ranges), 80)
	wantNum(t, run(t, "bucket_map", d, Inputs{"x": monthly(2026, 1, 2)}, ranges), 60)
	wantNum(t, run(t, "bucket_map", d, Inputs{"x": monthly(2026, 1, 9)}, ranges), 40)
	cats := `{"categories":{"cutting":80,"hold":50},"default":50}`
	wantNum(t, run(t, "bucket_map", d, Inputs{"x": cat(d, "cutting")}, cats), 80)
	wantNum(t, run(t, "bucket_map", d, Inputs{"x": cat(d, "hiking")}, cats), 50)
}

func TestThresholdConsecutive(t *testing.T) {
	d := me(2026, 3)
	wantBool(t, run(t, "threshold", d, Inputs{"x": monthly(2026, 3, 41)}, `{"op":">","value":40}`), true)
	wantBool(t, run(t, "threshold", d, Inputs{"x": monthly(2026, 3, 40)}, `{"op":">","value":40}`), false)
	in := Inputs{"x": monthly(2025, 12, 50, 91, 95, 92)}
	wantBool(t, run(t, "consecutive", d, in, `{"periods":3,"op":">=","value":90}`), true)
	wantBool(t, run(t, "consecutive", d, in, `{"periods":4,"op":">=","value":90}`), false)
	wantNull(t, run(t, "consecutive", me(2026, 1), in, `{"periods":3,"op":">=","value":90}`))
}

func TestFallback(t *testing.T) {
	d := me(2026, 1)
	p := `{"order":["a","b"],"default":50}`
	wantNum(t, run(t, "fallback", d, Inputs{"a": nil, "b": monthly(2026, 1, 7)}, p), 7)
	wantNum(t, run(t, "fallback", d, Inputs{"a": nil, "b": nil}, p), 50)
}

func TestRegistryValidateDerivation(t *testing.T) {
	r := NewDefaultRegistry()
	ok := catalog.Derivation{Fn: "percentile_rank", Inputs: map[string]string{"x": "spread.mid_large"},
		Params: json.RawMessage(`{"window_months":180,"min_obs":24}`), Version: 2}
	keys, err := r.ValidateDerivation(ok)
	if err != nil || len(keys) != 1 || keys[0] != "spread.mid_large" {
		t.Fatalf("got %v, %v", keys, err)
	}
	bad := []catalog.Derivation{
		{Fn: "nope", Version: 1},
		{Fn: "percentile_rank", Inputs: map[string]string{"y": "a"}, Params: ok.Params, Version: 1},
		{Fn: "percentile_rank", Inputs: ok.Inputs, Params: json.RawMessage(`{"window_months":0}`), Version: 1},
		{Fn: "percentile_rank", Inputs: ok.Inputs, Params: json.RawMessage(`{"window_months":12,"extra":1}`), Version: 1},
		{Fn: "percentile_rank", Inputs: ok.Inputs, Params: ok.Params, Version: 0},
		{Fn: "expr", Inputs: map[string]string{"a": "s.a"}, Params: json.RawMessage(`{"expr":"a + b"}`), Version: 1},
		{Fn: "fallback", Inputs: map[string]string{"a": "s.a", "b": "s.b"}, Params: json.RawMessage(`{"order":["a"]}`), Version: 1},
		{Fn: "rubric", Inputs: map[string]string{"a": "s.a"}, Params: json.RawMessage(`{"add":{"b":{"x":1}}}`), Version: 1},
		{Fn: "bucket_map", Inputs: map[string]string{"x": "s"}, Params: json.RawMessage(`{}`), Version: 1},
		{Fn: "threshold", Inputs: map[string]string{"x": "s"}, Params: json.RawMessage(`{"op":"~","value":1}`), Version: 1},
	}
	for i, d := range bad {
		if _, err := r.ValidateDerivation(d); err == nil {
			t.Errorf("case %d (%s): expected error", i, d.Fn)
		}
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate register did not panic")
		}
	}()
	r.Register(invert{})
}
