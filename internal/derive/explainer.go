package derive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"cloud.google.com/go/civil"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/calendar"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
)

// Explain limits: a score's formula tree is a handful of levels deep, but a
// request must stay bounded whatever the catalog holds.
const (
	maxExplainDepth = 8
	maxExplainNodes = 48
)

// SeriesGetter reads one series definition.
type SeriesGetter interface {
	Series(ctx context.Context, key string) (catalog.Series, error)
}

// ExplainerDeps are the explainer's collaborators.
type ExplainerDeps struct {
	Catalog    SeriesGetter
	Reader     datapoints.Reader
	Registry   Registry
	Clock      clock.Clock
	Governance Governance
}

type explainer struct{ ExplainerDeps }

// NewExplainer returns the formula-breakdown service. It recomputes each
// level with the current derivation from the inputs visible at the cutoff,
// exactly as the cascade does, and notes where that differs from the stored
// value (e.g. a value computed before a derivation change).
func NewExplainer(d ExplainerDeps) Explainer { return &explainer{d} }

type explainRun struct {
	o      datapoints.ReadOpts
	nodes  int
	series map[string]catalog.Series
}

func (e *explainer) Explain(ctx context.Context, key string, d civil.Date, o datapoints.ReadOpts) (Explanation, error) {
	if o.Cutoff.IsZero() {
		o.Cutoff = e.Clock.Now()
	}
	run := &explainRun{o: o, series: map[string]catalog.Series{}}
	s, err := run.get(ctx, e.Catalog, key)
	if err != nil {
		return Explanation{}, err
	}
	if d.IsZero() {
		latest, err := e.latest(ctx, key, o)
		if err != nil {
			return Explanation{}, err
		}
		if latest == nil {
			return Explanation{Series: s, Formula: formula(s.Derivation)}, nil
		}
		d = latest.ObsDate
	}
	return e.node(ctx, run, s, "", d, 0)
}

func (r *explainRun) get(ctx context.Context, c SeriesGetter, key string) (catalog.Series, error) {
	if s, ok := r.series[key]; ok {
		return s, nil
	}
	s, err := c.Series(ctx, key)
	if err != nil {
		return s, err
	}
	r.series[key] = s
	return s, nil
}

// latest is the series' newest observation visible at the cutoff, looking
// back in widening windows so daily series stay cheap.
func (e *explainer) latest(ctx context.Context, key string, o datapoints.ReadOpts) (*datapoints.Resolved, error) {
	upTo := civil.DateOf(o.Cutoff.UTC())
	for _, w := range []datapoints.Months{3, 36, 1200} {
		rows, err := e.Reader.History(ctx, key, upTo, w, o)
		if err != nil {
			return nil, err
		}
		if len(rows) > 0 {
			return &rows[len(rows)-1], nil
		}
	}
	return nil, nil
}

func (e *explainer) node(ctx context.Context, run *explainRun, s catalog.Series, alias string, d civil.Date, depth int) (Explanation, error) {
	run.nodes++
	ex := Explanation{Series: s, Alias: alias, ObsDate: d, Formula: formula(s.Derivation)}
	pt, err := e.Reader.At(ctx, s.Key, d, run.o)
	if err != nil {
		return ex, err
	}
	ex.Point = pt
	der := s.Derivation
	if der == nil {
		return ex, nil
	}
	p, ok := e.Registry.Get(der.Fn)
	if !ok {
		return ex, fmt.Errorf("%w: %q", ErrUnknownPrimitive, der.Fn)
	}

	// Load inputs the way the cascade does: the lookback window ending at d,
	// nothing later, at the caller's knowledge cutoff.
	look := int(p.Lookback(der.Params))
	from := d
	if look > 0 {
		from = calendar.WindowStart(d, look+1)
	}
	full := Inputs{}
	for a, key := range der.Inputs {
		rows, err := e.Reader.Range(ctx, key, from, d, run.o)
		if err != nil && !errors.Is(err, catalog.ErrNotFound) {
			return ex, err
		}
		full[a] = rows
	}
	in, _, _ := sliceInputs(full, d, look)
	steps, err := p.Explain(ctx, Env{Series: s, Governance: e.Governance}, d, in, der.Params)
	if err != nil {
		return ex, err
	}
	ex.Steps = steps

	for _, a := range sortedKeys(der.Inputs) {
		is, err := run.get(ctx, e.Catalog, der.Inputs[a])
		if err != nil {
			return ex, fmt.Errorf("input %s: %w", a, err)
		}
		inp := ExplainInput{Alias: a, Series: is}
		if h := in[a]; len(h) > 0 {
			last := h[len(h)-1]
			inp.Point = &last
		}
		if inp.Point != nil && is.Derivation != nil && depth+1 < maxExplainDepth && run.nodes < maxExplainNodes {
			part, err := e.node(ctx, run, is, a, inp.Point.ObsDate, depth+1)
			if err != nil {
				return ex, err
			}
			ex.Parts = append(ex.Parts, part)
			inp.Expanded = true
		}
		ex.Inputs = append(ex.Inputs, inp)
	}
	ex.Notes = drift(ex, der.Version)
	return ex, nil
}

// drift notes when today's formula does not reproduce the stored value.
func drift(ex Explanation, version int) []string {
	var got *float64
	if n := len(ex.Steps); n > 0 {
		got = ex.Steps[n-1].Result
	}
	switch {
	case ex.Point == nil && got != nil:
		return []string{fmt.Sprintf("No stored value at %s yet; the formula gives %s.", ex.ObsDate, show(*got))}
	case ex.Point == nil || ex.Point.Value.Num == nil || got == nil:
		return nil
	case math.Abs(*ex.Point.Value.Num-*got) > 1e-9*math.Max(1, math.Abs(*got)):
		return []string{fmt.Sprintf("Stored value %s was computed with derivation v%d; the current formula (v%d) gives %s.",
			show(*ex.Point.Value.Num), ex.Point.DerivVer, version, show(*got))}
	}
	return nil
}

// formula is a one-line description of a derivation.
func formula(der *catalog.Derivation) string {
	if der == nil {
		return ""
	}
	var params map[string]any
	_ = json.Unmarshal(der.Params, &params)
	if der.Fn == "expr" {
		if s, ok := params["expr"].(string); ok {
			return s
		}
	}
	args := make([]string, 0, len(der.Inputs))
	for _, a := range sortedKeys(der.Inputs) {
		args = append(args, a+" = "+der.Inputs[a])
	}
	out := der.Fn + "(" + strings.Join(args, ", ") + ")"
	if len(params) > 0 && der.Fn != "rubric" && der.Fn != "bucket_map" {
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		kv := make([]string, len(keys))
		for i, k := range keys {
			b, _ := json.Marshal(params[k])
			kv[i] = k + " " + string(b)
		}
		out += ", " + strings.Join(kv, ", ")
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
