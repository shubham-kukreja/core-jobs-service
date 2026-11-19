package model

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
)

// Registry looks up registered steps and constraints.
type Registry interface {
	Step(name string) (Step, bool)
	Constraint(name string) (Constraint, bool)
}

// Pipeline is the default Engine: a registry of steps and constraints run in
// the order a config lists them. It performs no I/O.
type Pipeline struct {
	mu          sync.RWMutex
	steps       map[string]Step
	constraints map[string]Constraint
}

var (
	_ Engine   = (*Pipeline)(nil)
	_ Registry = (*Pipeline)(nil)
)

// NewEngine returns a Pipeline with the built-in steps and constraints.
func NewEngine() *Pipeline {
	p := &Pipeline{steps: map[string]Step{}, constraints: map[string]Constraint{}}
	for _, s := range []Step{
		compositeStep{}, vetoStep{}, tiltStep{}, sectorQualifyStep{}, constraintsStep{p},
		normalizeStep{}, sleeveStep{}, rollupStep{},
	} {
		p.RegisterStep(s)
	}
	for _, c := range []Constraint{maxMultipleOfNeutral{}, extremeFullCap{}, sectorMaxTenure{}} {
		p.RegisterConstraint(c)
	}
	return p
}

// RegisterStep adds or replaces a step.
func (p *Pipeline) RegisterStep(s Step) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.steps[s.Name()] = s
}

// RegisterConstraint adds or replaces a constraint.
func (p *Pipeline) RegisterConstraint(c Constraint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.constraints[c.Name()] = c
}

// Step returns a registered step.
func (p *Pipeline) Step(name string) (Step, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	s, ok := p.steps[name]
	return s, ok
}

// Constraint returns a registered constraint.
func (p *Pipeline) Constraint(name string) (Constraint, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	c, ok := p.constraints[name]
	return c, ok
}

// Evaluate runs the config's steps over in. It reads nothing but in, so the
// same input always gives the same result. A config that fails the
// structural checks, or human params that break a rule (e.g. holding a
// sector that does not qualify), return invalid_params.
func (p *Pipeline) Evaluate(ctx context.Context, in EvalInput) (Result, error) {
	if err := CheckConfig(in.Config, p); err != nil {
		return Result{}, err
	}
	s := &State{In: in, Nodes: map[string]Node{}}
	for _, name := range in.Config.Steps {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		st, _ := p.Step(name)
		if err := st.Run(ctx, s); err != nil {
			return Result{}, err
		}
	}
	return Result{Nodes: s.Nodes, Allocation: s.Lines, Warnings: s.Warn}, nil
}

// ---- state helpers -----------------------------------------------------------

// Node keys are "<bucket>::<what>", e.g. "l1.debt::tilt".
func nodeKey(bucket, what string) string { return bucket + "::" + what }

// expand fills a series-key template.
func expand(tmpl, bucket, signal string) string {
	return strings.NewReplacer("{bucket}", bucket, "{signal}", signal).Replace(tmpl)
}

// put stores n, unless an active override pins it: then the override's
// value wins and the node is marked as overridden.
func (s *State) put(n Node) {
	for _, o := range s.In.Overrides {
		if o.NodeKey != n.Key || s.In.AsOf.Before(o.AsOf) || (o.ValidUntil != nil && s.In.AsOf.After(*o.ValidUntil)) {
			continue
		}
		switch {
		case n.Num != nil && o.Value.Num != nil:
			v := *o.Value.Num
			n.Formula = fmt.Sprintf("override %s (computed %s): %s", fmtF(v), fmtF(*n.Num), o.Reason)
			n.Num = &v
		case n.Bool != nil && o.Value.Bool != nil:
			v := *o.Value.Bool
			n.Formula = fmt.Sprintf("override %t (computed %t): %s", v, *n.Bool, o.Reason)
			n.Bool = &v
		default:
			continue
		}
		n.IsOverride = true
	}
	s.Nodes[n.Key] = n
}

func (s *State) num(key string) (float64, bool) {
	n, ok := s.Nodes[key]
	if !ok || n.Num == nil {
		return 0, false
	}
	return *n.Num, true
}

func (s *State) flag(key string) bool {
	n, ok := s.Nodes[key]
	return ok && n.Bool != nil && *n.Bool
}

// mustNum reads a node an earlier step must have written.
func (s *State) mustNum(key, step string) (float64, error) {
	v, ok := s.num(key)
	if !ok {
		return 0, apperr.New(apperr.InvalidParams, fmt.Sprintf("step %s needs node %s; check the step order", step, key),
			apperr.Detail{Path: "/steps", Reason: "step_order", Meta: map[string]any{"node": key}})
	}
	return v, nil
}

// score reads a score input; a missing one uses missing_score and warns.
func (s *State) score(series, node string) (float64, *Input) {
	if in, ok := s.In.Inputs[series]; ok && in.Value.Num != nil {
		return *in.Value.Num, &in
	}
	used := s.In.Config.MissingScore
	s.Warn = append(s.Warn, Warning{Code: "missing_score", Series: series, Node: node, Used: used})
	return used, nil
}

// trigger reads a veto trigger; missing means not triggered.
func (s *State) trigger(series string) (bool, *Input) {
	in, ok := s.In.Inputs[series]
	if !ok || in.Value.Bool == nil {
		return false, nil
	}
	return *in.Value.Bool, &in
}

func ptr[T any](v T) *T { return &v }

func fmtF(f float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", f), "0"), ".")
}

// rootScorecard is the one scorecard without a parent (L1).
func rootScorecard(c Config) (string, Scorecard) {
	for name, sc := range c.Scorecards {
		if sc.Parent == "" {
			return name, sc
		}
	}
	return "", Scorecard{}
}

// scorecardOrder lists the root first, then the rest by name.
func scorecardOrder(c Config) []string {
	root, _ := rootScorecard(c)
	out := []string{root}
	var rest []string
	for name := range c.Scorecards {
		if name != root {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// tiltOf is the workbook's capped tilt: (C − 50) / 50 × cap, within ±cap,
// and never above zero when vetoed.
func tiltOf(composite, limit float64, veto bool) float64 {
	t := math.Max(-limit, math.Min(limit, (composite-50)/50*limit))
	if veto {
		t = math.Min(0, t)
	}
	return t
}

// LineOrder lists allocation line items in rollup order: each root bucket's
// child buckets, then its sectors; a root bucket with no children is its
// own line.
func LineOrder(c Config) []string {
	_, root := rootScorecard(c)
	var out []string
	for _, g := range root.Buckets {
		n := len(out)
		for _, name := range scorecardOrder(c) {
			if sc := c.Scorecards[name]; sc.Parent == g {
				out = append(out, sc.Buckets...)
			}
		}
		if sat := c.SectorSatellite; sat != nil && sat.Parent == g {
			out = append(out, sat.Sectors...)
		}
		if len(out) == n {
			out = append(out, g)
		}
	}
	return out
}
