package derive

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"

	"cloud.google.com/go/civil"
	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"

	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/calendar"
)

// Builtins returns one instance of every built-in primitive.
func Builtins() []Primitive {
	return []Primitive{
		exprPrim{}, rollingMean{}, ratioOf{}, pctChange{}, diffOver{},
		percentileRank{}, invert{}, avgPrim{}, rubric{}, bucketMap{},
		constPrim{}, consecutive{}, threshold{}, fallback{},
	}
}

// ---- helpers ---------------------------------------------------------------

var null = datapoints.Value{}

// numAt returns alias's numeric value observed exactly on d.
func numAt(in Inputs, alias string, d civil.Date) (float64, bool) {
	h := in[alias]
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].ObsDate == d {
			if h[i].Value.Num == nil {
				return 0, false
			}
			return *h[i].Value.Num, true
		}
		if h[i].ObsDate.Before(d) {
			break
		}
	}
	return 0, false
}

// valAt returns alias's value observed exactly on d.
func valAt(in Inputs, alias string, d civil.Date) (datapoints.Value, bool) {
	for _, r := range in[alias] {
		if r.ObsDate == d {
			return r.Value, !r.Value.IsNull()
		}
	}
	return null, false
}

// numAsOf returns the latest numeric value with obs_date in [from, d].
func numAsOf(in Inputs, alias string, from, d civil.Date) (float64, bool) {
	h := in[alias]
	for i := len(h) - 1; i >= 0; i-- {
		r := h[i]
		if r.ObsDate.After(d) {
			continue
		}
		if r.ObsDate.Before(from) {
			break
		}
		if r.Value.Num != nil {
			return *r.Value.Num, true
		}
	}
	return 0, false
}

// window returns numeric values with obs_date in [from, d], oldest first.
// Anything after d is ignored even if a caller passes it: no look-ahead.
func window(in Inputs, alias string, from, d civil.Date) []float64 {
	var out []float64
	for _, r := range in[alias] {
		if r.ObsDate.Before(from) || r.ObsDate.After(d) || r.Value.Num == nil {
			continue
		}
		out = append(out, *r.Value.Num)
	}
	return out
}

// monthsBack moves d back n months, staying on month ends for month-end dates.
func monthsBack(d civil.Date, n int) civil.Date {
	p := calendar.AddMonths(d, -n)
	if calendar.IsMonthEnd(d) {
		p = calendar.MonthEnd(p)
	}
	return p
}

func finite(f float64) datapoints.Value {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return null
	}
	return datapoints.Num(f)
}

func compare(x float64, op string, k float64) (bool, error) {
	switch op {
	case ">":
		return x > k, nil
	case ">=":
		return x >= k, nil
	case "<":
		return x < k, nil
	case "<=":
		return x <= k, nil
	case "==":
		return x == k, nil
	case "!=":
		return x != k, nil
	}
	return false, fmt.Errorf("%w: unknown op %q", ErrInvalidParams, op)
}

func fmtNum(f float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", f), "0"), ".")
}

func fmtVal(v datapoints.Value) string {
	switch {
	case v.Num != nil:
		return fmtNum(*v.Num)
	case v.Bool != nil:
		return fmt.Sprint(*v.Bool)
	case v.Cat != nil:
		return *v.Cat
	}
	return "null"
}

// explainVia is the shared Explain: the inputs observed at d and the result.
func explainVia(p Primitive, ctx context.Context, env Env, d civil.Date, in Inputs, raw json.RawMessage) ([]ExplainStep, error) {
	aliases := make([]string, 0, len(in))
	for a := range in {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	parts := make([]string, 0, len(aliases))
	for _, a := range aliases {
		v, _ := valAt(in, a, d)
		parts = append(parts, a+" = "+fmtVal(v))
	}
	v, err := p.Compute(ctx, env, d, in, raw)
	if err != nil {
		return nil, err
	}
	step := ExplainStep{Text: fmt.Sprintf("%s(%s) = %s", p.Name(), strings.Join(parts, ", "), fmtVal(v)), Result: v.Num}
	return []ExplainStep{step}, nil
}

// ---- expr ------------------------------------------------------------------

type exprParams struct {
	Expr string `json:"expr"`
}

type exprPrim struct{}

var exprCache sync.Map // expression → *vm.Program

func (exprPrim) Name() string                               { return "expr" }
func (exprPrim) Aliases(json.RawMessage) []string           { return nil }
func (exprPrim) Lookback(json.RawMessage) datapoints.Months { return 0 }

func (exprPrim) ValidateParams(p json.RawMessage) error {
	var x exprParams
	if err := decodeParams(p, &x); err != nil {
		return err
	}
	if strings.TrimSpace(x.Expr) == "" {
		return fmt.Errorf("%w: expr is required", ErrInvalidParams)
	}
	return nil
}

func (exprPrim) ValidateInputs(p json.RawMessage, aliases []string) error {
	var x exprParams
	if err := decodeParams(p, &x); err != nil {
		return err
	}
	env := make(map[string]any, len(aliases))
	for _, a := range aliases {
		env[a] = 0.0
	}
	if _, err := expr.Compile(x.Expr, expr.Env(env)); err != nil {
		return fmt.Errorf("%w: expr: %w", ErrInvalidParams, err)
	}
	return nil
}

func compileExpr(src string, aliases []string) (*vm.Program, error) {
	key := src + "\x00" + strings.Join(aliases, ",")
	if p, ok := exprCache.Load(key); ok {
		return p.(*vm.Program), nil
	}
	env := make(map[string]any, len(aliases))
	for _, a := range aliases {
		env[a] = 0.0
	}
	prog, err := expr.Compile(src, expr.Env(env))
	if err != nil {
		return nil, fmt.Errorf("%w: expr: %w", ErrInvalidParams, err)
	}
	exprCache.Store(key, prog)
	return prog, nil
}

func (e exprPrim) Compute(_ context.Context, _ Env, d civil.Date, in Inputs, p json.RawMessage) (datapoints.Value, error) {
	var x exprParams
	if err := decodeParams(p, &x); err != nil {
		return null, err
	}
	aliases := make([]string, 0, len(in))
	env := make(map[string]any, len(in))
	for a := range in {
		f, ok := numAt(in, a, d)
		if !ok {
			return null, nil
		}
		aliases = append(aliases, a)
		env[a] = f
	}
	sort.Strings(aliases)
	prog, err := compileExpr(x.Expr, aliases)
	if err != nil {
		return null, err
	}
	out, err := expr.Run(prog, env)
	if err != nil {
		return null, fmt.Errorf("derive: expr: %w", err)
	}
	switch v := out.(type) {
	case float64:
		return finite(v), nil
	case int:
		return datapoints.Num(float64(v)), nil
	case bool:
		return datapoints.Bool(v), nil
	}
	return null, fmt.Errorf("%w: expr returned %T", ErrInvalidParams, out)
}

func (e exprPrim) Explain(ctx context.Context, env Env, d civil.Date, in Inputs, p json.RawMessage) ([]ExplainStep, error) {
	return explainVia(e, ctx, env, d, in, p)
}

// ---- rolling_mean ----------------------------------------------------------

type periodsParams struct {
	Periods int `json:"periods"`
}

func decodePeriods(p json.RawMessage) (periodsParams, error) {
	var x periodsParams
	if err := decodeParams(p, &x); err != nil {
		return x, err
	}
	if x.Periods < 1 || x.Periods > 600 {
		return x, fmt.Errorf("%w: periods must be 1..600", ErrInvalidParams)
	}
	return x, nil
}

type rollingMean struct{}

func (rollingMean) Name() string                     { return "rolling_mean" }
func (rollingMean) Aliases(json.RawMessage) []string { return []string{"x"} }
func (rollingMean) ValidateParams(p json.RawMessage) error {
	_, err := decodePeriods(p)
	return err
}
func (rollingMean) Lookback(p json.RawMessage) datapoints.Months {
	x, _ := decodePeriods(p)
	return datapoints.Months(x.Periods)
}

// Compute averages the last `periods` observations ending at d; it needs a
// value at d and a full window, otherwise null.
func (rollingMean) Compute(_ context.Context, _ Env, d civil.Date, in Inputs, p json.RawMessage) (datapoints.Value, error) {
	x, err := decodePeriods(p)
	if err != nil {
		return null, err
	}
	if _, ok := numAt(in, "x", d); !ok {
		return null, nil
	}
	vals := window(in, "x", calendar.WindowStart(d, x.Periods), d)
	if len(vals) < x.Periods {
		return null, nil
	}
	vals = vals[len(vals)-x.Periods:]
	var s float64
	for _, v := range vals {
		s += v
	}
	return finite(s / float64(len(vals))), nil
}

func (r rollingMean) Explain(ctx context.Context, env Env, d civil.Date, in Inputs, p json.RawMessage) ([]ExplainStep, error) {
	return explainVia(r, ctx, env, d, in, p)
}

// ---- ratio_of --------------------------------------------------------------

type ratioParams struct {
	Scale        *float64 `json:"scale"`
	MaxLagMonths int      `json:"max_lag_months"`
}

func decodeRatio(p json.RawMessage) (ratioParams, error) {
	var x ratioParams
	if err := decodeParams(p, &x); err != nil {
		return x, err
	}
	if x.MaxLagMonths < 0 || x.MaxLagMonths > 24 {
		return x, fmt.Errorf("%w: max_lag_months must be 0..24", ErrInvalidParams)
	}
	return x, nil
}

type ratioOf struct{}

func (ratioOf) Name() string                     { return "ratio_of" }
func (ratioOf) Aliases(json.RawMessage) []string { return []string{"a", "b"} }
func (ratioOf) ValidateParams(p json.RawMessage) error {
	_, err := decodeRatio(p)
	return err
}
func (ratioOf) Lookback(p json.RawMessage) datapoints.Months {
	x, _ := decodeRatio(p)
	return datapoints.Months(x.MaxLagMonths)
}

// Compute is scale × a(d) ÷ b, where b is aligned as-of: its latest value at
// most max_lag_months before d (exact date when 0).
func (ratioOf) Compute(_ context.Context, _ Env, d civil.Date, in Inputs, p json.RawMessage) (datapoints.Value, error) {
	x, err := decodeRatio(p)
	if err != nil {
		return null, err
	}
	a, ok := numAt(in, "a", d)
	if !ok {
		return null, nil
	}
	from := d
	if x.MaxLagMonths > 0 {
		from = monthsBack(d, x.MaxLagMonths)
	}
	b, ok := numAsOf(in, "b", from, d)
	if !ok || b == 0 {
		return null, nil
	}
	scale := 1.0
	if x.Scale != nil {
		scale = *x.Scale
	}
	return finite(scale * a / b), nil
}

func (r ratioOf) Explain(ctx context.Context, env Env, d civil.Date, in Inputs, p json.RawMessage) ([]ExplainStep, error) {
	return explainVia(r, ctx, env, d, in, p)
}

// ---- pct_change / diff_over --------------------------------------------------

type monthsParams struct {
	Months int      `json:"months"`
	Scale  *float64 `json:"scale"`
}

func decodeMonths(p json.RawMessage) (monthsParams, error) {
	var x monthsParams
	if err := decodeParams(p, &x); err != nil {
		return x, err
	}
	if x.Months < 1 || x.Months > 600 {
		return x, fmt.Errorf("%w: months must be 1..600", ErrInvalidParams)
	}
	return x, nil
}

// endpoints returns x at d and at d − months (exact observation dates).
func endpoints(in Inputs, d civil.Date, months int) (now, then float64, ok bool) {
	now, ok1 := numAt(in, "x", d)
	then, ok2 := numAt(in, "x", monthsBack(d, months))
	return now, then, ok1 && ok2
}

type pctChange struct{}

func (pctChange) Name() string                     { return "pct_change" }
func (pctChange) Aliases(json.RawMessage) []string { return []string{"x"} }
func (pctChange) ValidateParams(p json.RawMessage) error {
	_, err := decodeMonths(p)
	return err
}
func (pctChange) Lookback(p json.RawMessage) datapoints.Months {
	x, _ := decodeMonths(p)
	return datapoints.Months(x.Months)
}

// Compute is scale × (x(d) ÷ x(d − months) − 1); scale defaults to 100.
func (pctChange) Compute(_ context.Context, _ Env, d civil.Date, in Inputs, p json.RawMessage) (datapoints.Value, error) {
	x, err := decodeMonths(p)
	if err != nil {
		return null, err
	}
	now, then, ok := endpoints(in, d, x.Months)
	if !ok || then == 0 {
		return null, nil
	}
	scale := 100.0
	if x.Scale != nil {
		scale = *x.Scale
	}
	return finite(scale * (now/then - 1)), nil
}

func (c pctChange) Explain(ctx context.Context, env Env, d civil.Date, in Inputs, p json.RawMessage) ([]ExplainStep, error) {
	return explainVia(c, ctx, env, d, in, p)
}

type diffOver struct{}

func (diffOver) Name() string                     { return "diff_over" }
func (diffOver) Aliases(json.RawMessage) []string { return []string{"x"} }
func (diffOver) ValidateParams(p json.RawMessage) error {
	_, err := decodeMonths(p)
	return err
}
func (diffOver) Lookback(p json.RawMessage) datapoints.Months {
	x, _ := decodeMonths(p)
	return datapoints.Months(x.Months)
}

// Compute is scale × (x(d) − x(d − months)); scale defaults to 1.
func (diffOver) Compute(_ context.Context, _ Env, d civil.Date, in Inputs, p json.RawMessage) (datapoints.Value, error) {
	x, err := decodeMonths(p)
	if err != nil {
		return null, err
	}
	now, then, ok := endpoints(in, d, x.Months)
	if !ok {
		return null, nil
	}
	scale := 1.0
	if x.Scale != nil {
		scale = *x.Scale
	}
	return finite(scale * (now - then)), nil
}

func (c diffOver) Explain(ctx context.Context, env Env, d civil.Date, in Inputs, p json.RawMessage) ([]ExplainStep, error) {
	return explainVia(c, ctx, env, d, in, p)
}

// ---- percentile_rank ---------------------------------------------------------

type pctileParams struct {
	WindowMonths int `json:"window_months"`
	MinObs       int `json:"min_obs"`
}

func decodePctile(p json.RawMessage) (pctileParams, error) {
	var x pctileParams
	if err := decodeParams(p, &x); err != nil {
		return x, err
	}
	if x.WindowMonths < 1 || x.WindowMonths > 1200 {
		return x, fmt.Errorf("%w: window_months must be 1..1200", ErrInvalidParams)
	}
	if x.MinObs < 0 || x.MinObs == 1 {
		return x, fmt.Errorf("%w: min_obs must be 0 (governance default) or >= 2", ErrInvalidParams)
	}
	return x, nil
}

type percentileRank struct{}

func (percentileRank) Name() string                     { return "percentile_rank" }
func (percentileRank) Aliases(json.RawMessage) []string { return []string{"x"} }
func (percentileRank) ValidateParams(p json.RawMessage) error {
	_, err := decodePctile(p)
	return err
}
func (percentileRank) Lookback(p json.RawMessage) datapoints.Months {
	x, _ := decodePctile(p)
	return datapoints.Months(x.WindowMonths)
}

// Compute ranks x(d) among observations in the window ending at d, inclusive,
// as 100 × count(v < x) ÷ (n − 1) (Excel PERCENTRANK.INC). Observations after
// d are never read. Fewer than min_obs observations gives null.
func (percentileRank) Compute(_ context.Context, env Env, d civil.Date, in Inputs, p json.RawMessage) (datapoints.Value, error) {
	x, err := decodePctile(p)
	if err != nil {
		return null, err
	}
	cur, ok := numAt(in, "x", d)
	if !ok {
		return null, nil
	}
	minObs := x.MinObs
	if minObs == 0 {
		minObs = max(env.Governance.PercentileMinObservations, 2)
	}
	vals := window(in, "x", calendar.WindowStart(d, x.WindowMonths), d)
	if len(vals) < minObs {
		return null, nil
	}
	less := 0
	for _, v := range vals {
		if v < cur {
			less++
		}
	}
	return finite(100 * float64(less) / float64(len(vals)-1)), nil
}

func (r percentileRank) Explain(ctx context.Context, env Env, d civil.Date, in Inputs, p json.RawMessage) ([]ExplainStep, error) {
	x, err := decodePctile(p)
	if err != nil {
		return nil, err
	}
	steps, err := explainVia(r, ctx, env, d, in, p)
	if err != nil {
		return nil, err
	}
	n := len(window(in, "x", calendar.WindowStart(d, x.WindowMonths), d))
	steps = append([]ExplainStep{{Text: fmt.Sprintf("Window: %d observations over %d months ending %s", n, x.WindowMonths, d)}}, steps...)
	return steps, nil
}

// ---- invert ------------------------------------------------------------------

type invertParams struct {
	Scale *float64 `json:"scale"`
}

type invert struct{}

func (invert) Name() string                               { return "invert" }
func (invert) Aliases(json.RawMessage) []string           { return []string{"x"} }
func (invert) Lookback(json.RawMessage) datapoints.Months { return 0 }
func (invert) ValidateParams(p json.RawMessage) error {
	var x invertParams
	return decodeParams(p, &x)
}

// Compute is scale − x(d); scale defaults to 100.
func (invert) Compute(_ context.Context, _ Env, d civil.Date, in Inputs, p json.RawMessage) (datapoints.Value, error) {
	var x invertParams
	if err := decodeParams(p, &x); err != nil {
		return null, err
	}
	v, ok := numAt(in, "x", d)
	if !ok {
		return null, nil
	}
	scale := 100.0
	if x.Scale != nil {
		scale = *x.Scale
	}
	return finite(scale - v), nil
}

func (i invert) Explain(ctx context.Context, env Env, d civil.Date, in Inputs, p json.RawMessage) ([]ExplainStep, error) {
	return explainVia(i, ctx, env, d, in, p)
}

// ---- avg ---------------------------------------------------------------------

type avgPrim struct{}

func (avgPrim) Name() string                               { return "avg" }
func (avgPrim) Aliases(json.RawMessage) []string           { return nil }
func (avgPrim) Lookback(json.RawMessage) datapoints.Months { return 0 }
func (avgPrim) ValidateParams(p json.RawMessage) error {
	return decodeParams(p, &struct{}{})
}

// Compute is the mean of the inputs present at d; null when none are.
func (avgPrim) Compute(_ context.Context, _ Env, d civil.Date, in Inputs, _ json.RawMessage) (datapoints.Value, error) {
	var s float64
	n := 0
	for a := range in {
		if v, ok := numAt(in, a, d); ok {
			s += v
			n++
		}
	}
	if n == 0 {
		return null, nil
	}
	return finite(s / float64(n)), nil
}

func (a avgPrim) Explain(ctx context.Context, env Env, d civil.Date, in Inputs, p json.RawMessage) ([]ExplainStep, error) {
	return explainVia(a, ctx, env, d, in, p)
}

// ---- rubric ------------------------------------------------------------------

type rubricParams struct {
	Base  float64                       `json:"base"`
	Clamp []float64                     `json:"clamp"`
	Add   map[string]map[string]float64 `json:"add"`
}

func decodeRubric(p json.RawMessage) (rubricParams, error) {
	var x rubricParams
	if err := decodeParams(p, &x); err != nil {
		return x, err
	}
	if len(x.Add) == 0 {
		return x, fmt.Errorf("%w: rubric needs add", ErrInvalidParams)
	}
	if x.Clamp != nil && (len(x.Clamp) != 2 || x.Clamp[0] > x.Clamp[1]) {
		return x, fmt.Errorf("%w: clamp must be [lo, hi]", ErrInvalidParams)
	}
	return x, nil
}

type rubric struct{}

func (rubric) Name() string                               { return "rubric" }
func (rubric) Lookback(json.RawMessage) datapoints.Months { return 0 }
func (rubric) ValidateParams(p json.RawMessage) error {
	_, err := decodeRubric(p)
	return err
}
func (rubric) Aliases(p json.RawMessage) []string {
	x, err := decodeRubric(p)
	if err != nil {
		return []string{}
	}
	out := make([]string, 0, len(x.Add))
	for a := range x.Add {
		out = append(out, a)
	}
	return out
}

// Compute is base + Σ add[alias][category(d)], clamped. Any missing input
// gives null; an unknown category is an error so bad data is not scored.
func (rubric) Compute(_ context.Context, _ Env, d civil.Date, in Inputs, p json.RawMessage) (datapoints.Value, error) {
	x, err := decodeRubric(p)
	if err != nil {
		return null, err
	}
	score := x.Base
	for a, table := range x.Add {
		v, ok := valAt(in, a, d)
		if !ok || v.Cat == nil {
			return null, nil
		}
		add, ok := table[*v.Cat]
		if !ok {
			return null, fmt.Errorf("%w: rubric %s has no category %q", ErrInvalidParams, a, *v.Cat)
		}
		score += add
	}
	if x.Clamp != nil {
		score = math.Min(math.Max(score, x.Clamp[0]), x.Clamp[1])
	}
	return finite(score), nil
}

func (r rubric) Explain(ctx context.Context, env Env, d civil.Date, in Inputs, p json.RawMessage) ([]ExplainStep, error) {
	return explainVia(r, ctx, env, d, in, p)
}

// ---- bucket_map ----------------------------------------------------------------

type bucketRange struct {
	Min   *float64 `json:"min"` // inclusive
	Max   *float64 `json:"max"` // exclusive
	Score float64  `json:"score"`
}

type bucketParams struct {
	Ranges     []bucketRange      `json:"ranges"`
	Categories map[string]float64 `json:"categories"`
	Default    *float64           `json:"default"`
}

func decodeBucket(p json.RawMessage) (bucketParams, error) {
	var x bucketParams
	if err := decodeParams(p, &x); err != nil {
		return x, err
	}
	if (len(x.Ranges) == 0) == (len(x.Categories) == 0) {
		return x, fmt.Errorf("%w: bucket_map needs exactly one of ranges or categories", ErrInvalidParams)
	}
	for i, r := range x.Ranges {
		if r.Min != nil && r.Max != nil && *r.Min >= *r.Max {
			return x, fmt.Errorf("%w: range %d has min >= max", ErrInvalidParams, i)
		}
	}
	return x, nil
}

type bucketMap struct{}

func (bucketMap) Name() string                               { return "bucket_map" }
func (bucketMap) Aliases(json.RawMessage) []string           { return []string{"x"} }
func (bucketMap) Lookback(json.RawMessage) datapoints.Months { return 0 }
func (bucketMap) ValidateParams(p json.RawMessage) error {
	_, err := decodeBucket(p)
	return err
}

// Compute returns the score of the first range containing x (min ≤ x < max)
// or of x's category; default (or null) when nothing matches.
func (bucketMap) Compute(_ context.Context, _ Env, d civil.Date, in Inputs, p json.RawMessage) (datapoints.Value, error) {
	x, err := decodeBucket(p)
	if err != nil {
		return null, err
	}
	v, ok := valAt(in, "x", d)
	if !ok {
		return null, nil
	}
	switch {
	case len(x.Ranges) > 0 && v.Num != nil:
		for _, r := range x.Ranges {
			if (r.Min == nil || *v.Num >= *r.Min) && (r.Max == nil || *v.Num < *r.Max) {
				return datapoints.Num(r.Score), nil
			}
		}
	case len(x.Categories) > 0 && v.Cat != nil:
		if s, ok := x.Categories[*v.Cat]; ok {
			return datapoints.Num(s), nil
		}
	}
	if x.Default != nil {
		return datapoints.Num(*x.Default), nil
	}
	return null, nil
}

func (b bucketMap) Explain(ctx context.Context, env Env, d civil.Date, in Inputs, p json.RawMessage) ([]ExplainStep, error) {
	return explainVia(b, ctx, env, d, in, p)
}

// ---- const ---------------------------------------------------------------------

type constParams struct {
	Value *float64 `json:"value"`
}

type constPrim struct{}

func (constPrim) Name() string                               { return "const" }
func (constPrim) Aliases(json.RawMessage) []string           { return []string{} }
func (constPrim) Lookback(json.RawMessage) datapoints.Months { return 0 }
func (constPrim) ValidateParams(p json.RawMessage) error {
	var x constParams
	if err := decodeParams(p, &x); err != nil {
		return err
	}
	if x.Value == nil {
		return fmt.Errorf("%w: const needs value", ErrInvalidParams)
	}
	return nil
}

func (constPrim) Compute(_ context.Context, _ Env, _ civil.Date, _ Inputs, p json.RawMessage) (datapoints.Value, error) {
	var x constParams
	if err := decodeParams(p, &x); err != nil || x.Value == nil {
		return null, fmt.Errorf("%w: const needs value", ErrInvalidParams)
	}
	return datapoints.Num(*x.Value), nil
}

func (c constPrim) Explain(ctx context.Context, env Env, d civil.Date, in Inputs, p json.RawMessage) ([]ExplainStep, error) {
	return explainVia(c, ctx, env, d, in, p)
}

// ---- threshold / consecutive -----------------------------------------------------

type condParams struct {
	Op      string   `json:"op"`
	Value   *float64 `json:"value"`
	Periods int      `json:"periods"`
}

func decodeCond(p json.RawMessage, needPeriods bool) (condParams, error) {
	var x condParams
	if err := decodeParams(p, &x); err != nil {
		return x, err
	}
	if x.Op != "" {
		if _, err := compare(0, x.Op, 0); err != nil {
			return x, err
		}
		if x.Value == nil {
			return x, fmt.Errorf("%w: op needs value", ErrInvalidParams)
		}
	} else if !needPeriods {
		return x, fmt.Errorf("%w: threshold needs op and value", ErrInvalidParams)
	}
	if needPeriods && (x.Periods < 1 || x.Periods > 120) {
		return x, fmt.Errorf("%w: periods must be 1..120", ErrInvalidParams)
	}
	if !needPeriods && x.Periods != 0 {
		return x, fmt.Errorf("%w: threshold takes no periods", ErrInvalidParams)
	}
	return x, nil
}

// holds evaluates the condition on one resolved value: numeric with op, or a
// bool input as-is when op is empty.
func holds(x condParams, v datapoints.Value) (bool, bool) {
	if x.Op == "" {
		if v.Bool == nil {
			return false, false
		}
		return *v.Bool, true
	}
	if v.Num == nil {
		return false, false
	}
	ok, _ := compare(*v.Num, x.Op, *x.Value)
	return ok, true
}

type threshold struct{}

func (threshold) Name() string                               { return "threshold" }
func (threshold) Aliases(json.RawMessage) []string           { return []string{"x"} }
func (threshold) Lookback(json.RawMessage) datapoints.Months { return 0 }
func (threshold) ValidateParams(p json.RawMessage) error {
	_, err := decodeCond(p, false)
	return err
}

// Compute is x(d) op value, as a bool.
func (threshold) Compute(_ context.Context, _ Env, d civil.Date, in Inputs, p json.RawMessage) (datapoints.Value, error) {
	x, err := decodeCond(p, false)
	if err != nil {
		return null, err
	}
	v, ok := valAt(in, "x", d)
	if !ok {
		return null, nil
	}
	b, ok := holds(x, v)
	if !ok {
		return null, nil
	}
	return datapoints.Bool(b), nil
}

func (t threshold) Explain(ctx context.Context, env Env, d civil.Date, in Inputs, p json.RawMessage) ([]ExplainStep, error) {
	return explainVia(t, ctx, env, d, in, p)
}

type consecutive struct{}

func (consecutive) Name() string                     { return "consecutive" }
func (consecutive) Aliases(json.RawMessage) []string { return []string{"x"} }
func (consecutive) ValidateParams(p json.RawMessage) error {
	_, err := decodeCond(p, true)
	return err
}
func (consecutive) Lookback(p json.RawMessage) datapoints.Months {
	x, _ := decodeCond(p, true)
	return datapoints.Months(x.Periods)
}

// Compute is true when the condition held on each of the last `periods`
// observations ending at d. Null when d has no value or history is short.
func (consecutive) Compute(_ context.Context, _ Env, d civil.Date, in Inputs, p json.RawMessage) (datapoints.Value, error) {
	x, err := decodeCond(p, true)
	if err != nil {
		return null, err
	}
	if _, ok := valAt(in, "x", d); !ok {
		return null, nil
	}
	from := calendar.WindowStart(d, x.Periods)
	var obs []datapoints.Value
	for _, r := range in["x"] {
		if !r.ObsDate.Before(from) && !r.ObsDate.After(d) && !r.Value.IsNull() {
			obs = append(obs, r.Value)
		}
	}
	if len(obs) < x.Periods {
		return null, nil
	}
	for _, v := range obs[len(obs)-x.Periods:] {
		b, ok := holds(x, v)
		if !ok {
			return null, nil
		}
		if !b {
			return datapoints.Bool(false), nil
		}
	}
	return datapoints.Bool(true), nil
}

func (c consecutive) Explain(ctx context.Context, env Env, d civil.Date, in Inputs, p json.RawMessage) ([]ExplainStep, error) {
	return explainVia(c, ctx, env, d, in, p)
}

// ---- fallback ------------------------------------------------------------------

type fallbackParams struct {
	Order   []string `json:"order"`
	Default *float64 `json:"default"`
}

type fallback struct{}

func (fallback) Name() string                               { return "fallback" }
func (fallback) Aliases(json.RawMessage) []string           { return nil }
func (fallback) Lookback(json.RawMessage) datapoints.Months { return 0 }
func (fallback) ValidateParams(p json.RawMessage) error {
	var x fallbackParams
	if err := decodeParams(p, &x); err != nil {
		return err
	}
	if len(x.Order) == 0 {
		return fmt.Errorf("%w: fallback needs order", ErrInvalidParams)
	}
	return nil
}

func (fallback) ValidateInputs(p json.RawMessage, aliases []string) error {
	var x fallbackParams
	if err := decodeParams(p, &x); err != nil {
		return err
	}
	o := slices.Clone(x.Order)
	sort.Strings(o)
	if !slices.Equal(o, aliases) {
		return fmt.Errorf("%w: fallback order %v must list each input once (%v)", ErrInvalidParams, x.Order, aliases)
	}
	return nil
}

// Compute returns the first input in order with a numeric value at d, else
// default (e.g. 50: "a missing signal scores 50"), else null.
func (fallback) Compute(_ context.Context, _ Env, d civil.Date, in Inputs, p json.RawMessage) (datapoints.Value, error) {
	var x fallbackParams
	if err := decodeParams(p, &x); err != nil {
		return null, err
	}
	for _, a := range x.Order {
		if v, ok := numAt(in, a, d); ok {
			return datapoints.Num(v), nil
		}
	}
	if x.Default != nil {
		return datapoints.Num(*x.Default), nil
	}
	return null, nil
}

func (f fallback) Explain(ctx context.Context, env Env, d civil.Date, in Inputs, p json.RawMessage) ([]ExplainStep, error) {
	return explainVia(f, ctx, env, d, in, p)
}
