package derive

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"cloud.google.com/go/civil"

	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/calendar"
)

// The Explain implementations below turn one computation into the lines the
// Data Points screen shows, e.g. "PERCENTRANK(180 readings over 180m, 19.27)
// × 100 = 99.47". Each one calls Compute for the result, so the breakdown
// can never disagree with the stored number's formula.

// show formats a number for a breakdown: at most 2 decimals.
func show(f float64) string {
	s := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", f), "0"), ".")
	if s == "-0" {
		return "0"
	}
	return s
}

// showVal formats any value for a breakdown.
func showVal(v datapoints.Value) string {
	if v.Num != nil {
		return show(*v.Num)
	}
	return fmtVal(v)
}

// result is the final step: "<text> = <value>".
func result(text string, v datapoints.Value) ExplainStep {
	if v.IsNull() {
		return ExplainStep{Text: text + " → no value"}
	}
	return ExplainStep{Text: text + " = " + showVal(v), Result: v.Num}
}

// computed runs p.Compute and appends the final step.
func computed(ctx context.Context, p Primitive, env Env, d civil.Date, in Inputs, raw json.RawMessage, steps []ExplainStep, text string) ([]ExplainStep, error) {
	v, err := p.Compute(ctx, env, d, in, raw)
	if err != nil {
		return nil, err
	}
	return append(steps, result(text, v)), nil
}

func numOrMissing(in Inputs, alias string, d civil.Date) string {
	if v, ok := numAt(in, alias, d); ok {
		return show(v)
	}
	return "missing"
}

var identRE = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

func explainExpr(ctx context.Context, p Primitive, env Env, d civil.Date, in Inputs, raw json.RawMessage) ([]ExplainStep, error) {
	var x exprParams
	if err := decodeParams(raw, &x); err != nil {
		return nil, err
	}
	sub := identRE.ReplaceAllStringFunc(x.Expr, func(id string) string {
		if _, ok := in[id]; !ok {
			return id
		}
		if v, ok := valAt(in, id, d); ok {
			return showVal(v)
		}
		return "missing"
	})
	text := x.Expr
	if sub != x.Expr {
		text += " = " + sub
	}
	return computed(ctx, p, env, d, in, raw, nil, text)
}

func explainRollingMean(ctx context.Context, p Primitive, env Env, d civil.Date, in Inputs, raw json.RawMessage) ([]ExplainStep, error) {
	x, err := decodePeriods(raw)
	if err != nil {
		return nil, err
	}
	vals := window(in, calendar.WindowStart(d, x.Periods), d)
	if len(vals) > x.Periods {
		vals = vals[len(vals)-x.Periods:]
	}
	text := fmt.Sprintf("Mean of the last %d observations to %s", x.Periods, d)
	if len(vals) < x.Periods {
		text = fmt.Sprintf("Mean of the last %d observations to %s (only %d available)", x.Periods, d, len(vals))
	} else if len(vals) <= 12 {
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = show(v)
		}
		text = fmt.Sprintf("(%s) / %d", strings.Join(parts, " + "), len(vals))
	}
	return computed(ctx, p, env, d, in, raw, nil, text)
}

func explainRatio(ctx context.Context, p Primitive, env Env, d civil.Date, in Inputs, raw json.RawMessage) ([]ExplainStep, error) {
	x, err := decodeRatio(raw)
	if err != nil {
		return nil, err
	}
	from := d
	if x.MaxLagMonths > 0 {
		from = monthsBack(d, x.MaxLagMonths)
	}
	b := "missing"
	if v, ok := numAsOf(in, "b", from, d); ok {
		b = show(v)
	}
	text := fmt.Sprintf("a ÷ b = %s ÷ %s", numOrMissing(in, "a", d), b)
	if x.Scale != nil && *x.Scale != 1 {
		text = fmt.Sprintf("%s × a ÷ b = %s × %s ÷ %s", show(*x.Scale), show(*x.Scale), numOrMissing(in, "a", d), b)
	}
	var steps []ExplainStep
	if x.MaxLagMonths > 0 {
		steps = append(steps, ExplainStep{Text: fmt.Sprintf("b is its latest value from %s to %s", from, d)})
	}
	return computed(ctx, p, env, d, in, raw, steps, text)
}

func explainChange(ctx context.Context, p Primitive, env Env, d civil.Date, in Inputs, raw json.RawMessage, pct bool) ([]ExplainStep, error) {
	x, err := decodeMonths(raw)
	if err != nil {
		return nil, err
	}
	then := monthsBack(d, x.Months)
	now, prev := numOrMissing(in, "x", d), numOrMissing(in, "x", then)
	var text string
	if pct {
		scale := 100.0
		if x.Scale != nil {
			scale = *x.Scale
		}
		text = fmt.Sprintf("%s × (x(%s) ÷ x(%s) − 1) = %s × (%s ÷ %s − 1)", show(scale), d, then, show(scale), now, prev)
	} else {
		text = fmt.Sprintf("x(%s) − x(%s) = %s − %s", d, then, now, prev)
		if x.Scale != nil && *x.Scale != 1 {
			text = fmt.Sprintf("%s × (x(%s) − x(%s)) = %s × (%s − %s)", show(*x.Scale), d, then, show(*x.Scale), now, prev)
		}
	}
	return computed(ctx, p, env, d, in, raw, nil, text)
}

func explainPercentile(ctx context.Context, p Primitive, env Env, d civil.Date, in Inputs, raw json.RawMessage) ([]ExplainStep, error) {
	x, err := decodePctile(raw)
	if err != nil {
		return nil, err
	}
	from := calendar.WindowStart(d, x.WindowMonths)
	n := len(window(in, from, d))
	minObs := x.MinObs
	if minObs == 0 {
		minObs = max(env.Governance.PercentileMinObservations, 2)
	}
	steps := []ExplainStep{{Text: fmt.Sprintf("Window: %d readings from %s to %s (%d months; at least %d needed)", n, from, d, x.WindowMonths, minObs)}}
	return computed(ctx, p, env, d, in, raw, steps,
		fmt.Sprintf("PERCENTRANK(%d readings, %s) × 100", n, numOrMissing(in, "x", d)))
}

func explainInvert(ctx context.Context, p Primitive, env Env, d civil.Date, in Inputs, raw json.RawMessage) ([]ExplainStep, error) {
	var x invertParams
	if err := decodeParams(raw, &x); err != nil {
		return nil, err
	}
	scale := 100.0
	if x.Scale != nil {
		scale = *x.Scale
	}
	return computed(ctx, p, env, d, in, raw, nil, fmt.Sprintf("%s − %s", show(scale), numOrMissing(in, "x", d)))
}

func explainAvg(ctx context.Context, p Primitive, env Env, d civil.Date, in Inputs, raw json.RawMessage) ([]ExplainStep, error) {
	var present []string
	var missing []string
	for _, a := range sortedAliases(in) {
		if v, ok := numAt(in, a, d); ok {
			present = append(present, show(v))
		} else {
			missing = append(missing, a)
		}
	}
	var steps []ExplainStep
	if len(missing) > 0 {
		steps = append(steps, ExplainStep{Text: "Left out (no value): " + strings.Join(missing, ", ")})
	}
	text := "mean of no inputs"
	if len(present) > 0 {
		text = fmt.Sprintf("(%s) / %d", strings.Join(present, " + "), len(present))
	}
	return computed(ctx, p, env, d, in, raw, steps, text)
}

func explainRubric(ctx context.Context, p Primitive, env Env, d civil.Date, in Inputs, raw json.RawMessage) ([]ExplainStep, error) {
	x, err := decodeRubric(raw)
	if err != nil {
		return nil, err
	}
	steps := []ExplainStep{{Text: "Base " + show(x.Base)}}
	terms := []string{show(x.Base)}
	aliases := make([]string, 0, len(x.Add))
	for a := range x.Add {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	for _, a := range aliases {
		v, ok := valAt(in, a, d)
		if !ok || v.Cat == nil {
			steps = append(steps, ExplainStep{Text: a + ": no answer"})
			continue
		}
		add := x.Add[a][*v.Cat]
		steps = append(steps, ExplainStep{Text: fmt.Sprintf("%s = %s → %s", a, *v.Cat, signed(add))})
		terms = append(terms, signed(add))
	}
	text := strings.Join(terms, " ")
	if x.Clamp != nil {
		text = fmt.Sprintf("clamp(%s, %s, %s)", text, show(x.Clamp[0]), show(x.Clamp[1]))
	}
	return computed(ctx, p, env, d, in, raw, steps, text)
}

func signed(f float64) string {
	if f < 0 {
		return "− " + show(math.Abs(f))
	}
	return "+ " + show(f)
}

func explainBucket(ctx context.Context, p Primitive, env Env, d civil.Date, in Inputs, raw json.RawMessage) ([]ExplainStep, error) {
	x, err := decodeBucket(raw)
	if err != nil {
		return nil, err
	}
	v, ok := valAt(in, "x", d)
	text := "x is missing"
	if ok {
		text = "x = " + showVal(v)
		matched := false
		switch {
		case len(x.Ranges) > 0 && v.Num != nil:
			for _, r := range x.Ranges {
				if (r.Min == nil || *v.Num >= *r.Min) && (r.Max == nil || *v.Num < *r.Max) {
					text += fmt.Sprintf(" falls in [%s, %s)", bound(r.Min, "−∞"), bound(r.Max, "∞"))
					matched = true
					break
				}
			}
		case len(x.Categories) > 0 && v.Cat != nil:
			_, matched = x.Categories[*v.Cat]
		}
		if !matched && x.Default != nil {
			text += " matches no bucket; default"
		}
	}
	return computed(ctx, p, env, d, in, raw, nil, text)
}

func bound(f *float64, inf string) string {
	if f == nil {
		return inf
	}
	return show(*f)
}

func explainConst(ctx context.Context, p Primitive, env Env, d civil.Date, in Inputs, raw json.RawMessage) ([]ExplainStep, error) {
	return computed(ctx, p, env, d, in, raw, nil, "Constant")
}

func condText(x condParams) string {
	if x.Op == "" {
		return "x is true"
	}
	return fmt.Sprintf("x %s %s", x.Op, show(*x.Value))
}

func explainThreshold(ctx context.Context, p Primitive, env Env, d civil.Date, in Inputs, raw json.RawMessage) ([]ExplainStep, error) {
	x, err := decodeCond(raw, false)
	if err != nil {
		return nil, err
	}
	cur := "missing"
	if v, ok := valAt(in, "x", d); ok {
		cur = showVal(v)
	}
	return computed(ctx, p, env, d, in, raw, nil, fmt.Sprintf("%s, with x = %s", condText(x), cur))
}

func explainConsecutive(ctx context.Context, p Primitive, env Env, d civil.Date, in Inputs, raw json.RawMessage) ([]ExplainStep, error) {
	x, err := decodeCond(raw, true)
	if err != nil {
		return nil, err
	}
	from := calendar.WindowStart(d, x.Periods)
	var obs []string
	for _, r := range in["x"] {
		if !r.ObsDate.Before(from) && !r.ObsDate.After(d) && !r.Value.IsNull() {
			obs = append(obs, showVal(r.Value))
		}
	}
	if len(obs) > x.Periods {
		obs = obs[len(obs)-x.Periods:]
	}
	return computed(ctx, p, env, d, in, raw, nil,
		fmt.Sprintf("%s on each of the last %d observations [%s]", condText(x), x.Periods, strings.Join(obs, ", ")))
}

func explainFallback(ctx context.Context, p Primitive, env Env, d civil.Date, in Inputs, raw json.RawMessage) ([]ExplainStep, error) {
	var x fallbackParams
	if err := decodeParams(raw, &x); err != nil {
		return nil, err
	}
	var steps []ExplainStep
	text := "no input has a value"
	for _, a := range x.Order {
		if v, ok := numAt(in, a, d); ok {
			text = fmt.Sprintf("first available: %s = %s", a, show(v))
			break
		}
		steps = append(steps, ExplainStep{Text: a + ": no value, next"})
	}
	if text == "no input has a value" && x.Default != nil {
		text += "; default"
	}
	return computed(ctx, p, env, d, in, raw, steps, text)
}

func sortedAliases(in Inputs) []string {
	out := make([]string, 0, len(in))
	for a := range in {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}
