package model

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
)

// The built-in steps follow the workbook: the scoring sheets (composite,
// veto, tilt, normalize), Sector Satellite (sector_qualify) and Portfolio
// Rollup (sleeve, rollup). Each writes nodes "<bucket>::<what>".

// ---- composite ---------------------------------------------------------------

type compositeStep struct{}

func (compositeStep) Name() string { return "composite" }

// Run writes C_b = Σ w_i s_{b,i} for every bucket and sector.
func (compositeStep) Run(_ context.Context, s *State) error {
	c := s.In.Config
	for _, name := range scorecardOrder(c) {
		sc := c.Scorecards[name]
		for _, b := range sc.Buckets {
			s.composite(b, sc.ScoreSeries, sc.Signals)
		}
	}
	if sat := c.SectorSatellite; sat != nil {
		for _, sec := range sat.Sectors {
			s.composite(sec, sat.ScoreSeries, sat.Signals)
		}
	}
	return nil
}

func (s *State) composite(bucket, tmpl string, sigs []Signal) {
	var sum float64
	in := NodeInputs{Params: map[string]float64{}}
	terms := make([]string, 0, len(sigs))
	for _, sg := range sigs {
		v, inp := s.score(expand(tmpl, bucket, sg.Key), bucket)
		if inp != nil && inp.DatapointID != 0 {
			in.Datapoints = append(in.Datapoints, inp.DatapointID)
		}
		in.Params[sg.Key] = sg.Weight
		sum += sg.Weight * v
		terms = append(terms, fmt.Sprintf("%s×%s", fmtF(sg.Weight), fmtF(v)))
	}
	s.put(Node{Key: nodeKey(bucket, "composite"), Num: ptr(sum), Step: "composite",
		Formula: strings.Join(terms, " + ") + " = " + fmtF(sum), Inputs: in})
}

// ---- veto --------------------------------------------------------------------

type vetoStep struct{}

func (vetoStep) Name() string { return "veto" }

// Run sets "<bucket>::veto" when any of the bucket's triggers is true.
func (vetoStep) Run(_ context.Context, s *State) error {
	c := s.In.Config
	for _, name := range scorecardOrder(c) {
		sc := c.Scorecards[name]
		for _, b := range sc.Buckets {
			s.veto(b, sc.Vetoes)
		}
	}
	if sat := c.SectorSatellite; sat != nil {
		for _, sec := range sat.Sectors {
			s.veto(sec, sat.Vetoes)
		}
	}
	return nil
}

func (s *State) veto(bucket string, vetoes map[string][]string) {
	var active []string
	var in NodeInputs
	for _, t := range append(slices.Clone(vetoes["*"]), vetoes[bucket]...) {
		key := expand(t, bucket, "")
		on, inp := s.trigger(key)
		if inp != nil && inp.DatapointID != 0 {
			in.Datapoints = append(in.Datapoints, inp.DatapointID)
		}
		if on {
			active = append(active, key)
		}
	}
	f := "no trigger active"
	if len(active) > 0 {
		f = "triggered: " + strings.Join(active, ", ")
	}
	s.put(Node{Key: nodeKey(bucket, "veto"), Bool: ptr(len(active) > 0), Step: "veto", Formula: f, Inputs: in})
}

// ---- tilt --------------------------------------------------------------------

type tiltStep struct{}

func (tiltStep) Name() string { return "tilt" }

// Run writes t_b = clamp((C_b − 50) / 50 × k, ±k), capped at 0 when vetoed.
func (tiltStep) Run(_ context.Context, s *State) error {
	c := s.In.Config
	for _, name := range scorecardOrder(c) {
		sc := c.Scorecards[name]
		for _, b := range sc.Buckets {
			comp, err := s.mustNum(nodeKey(b, "composite"), "tilt")
			if err != nil {
				return err
			}
			s.putTilt(b, comp, sc.TiltCap, "")
		}
	}
	return nil
}

// putTilt writes a bucket's tilt with the given cap; note explains a cap
// other than the scorecard's.
func (s *State) putTilt(bucket string, comp, limit float64, note string) {
	veto := s.flag(nodeKey(bucket, "veto"))
	t := tiltOf(comp, limit, veto)
	f := fmt.Sprintf("clamp((%s − 50) / 50 × %s, ±%s) = %s", fmtF(comp), fmtF(limit), fmtF(limit), fmtF(t))
	if veto {
		f = fmt.Sprintf("min(0, clamp((%s − 50) / 50 × %s, ±%s)) = %s (vetoed)", fmtF(comp), fmtF(limit), fmtF(limit), fmtF(t))
	}
	if note != "" {
		f += "; " + note
	}
	s.put(Node{Key: nodeKey(bucket, "tilt"), Num: ptr(t), Step: "tilt", Formula: f, Inputs: NodeInputs{
		Nodes: []string{nodeKey(bucket, "composite"), nodeKey(bucket, "veto")}, Params: map[string]float64{"cap": limit},
	}})
}

// ---- sector_qualify ----------------------------------------------------------

type sectorQualifyStep struct{}

func (sectorQualifyStep) Name() string { return "sector_qualify" }

// Run marks sectors that clear the threshold and are not vetoed, and checks
// the human choice of sectors held: each must qualify, at most max_sectors.
func (sectorQualifyStep) Run(_ context.Context, s *State) error {
	sat := s.In.Config.SectorSatellite
	if sat == nil {
		if len(s.In.Params.SectorsHeld) > 0 {
			return heldErr("no_satellite", "The model has no sector satellite", nil)
		}
		return nil
	}
	for _, sec := range sat.Sectors {
		comp, err := s.mustNum(nodeKey(sec, "composite"), "sector_qualify")
		if err != nil {
			return err
		}
		veto := s.flag(nodeKey(sec, "veto"))
		q := !veto && comp >= sat.QualifyThreshold
		f := fmt.Sprintf("%s ≥ %s", fmtF(comp), fmtF(sat.QualifyThreshold))
		switch {
		case veto:
			f = "vetoed"
		case !q:
			f = fmt.Sprintf("%s < %s", fmtF(comp), fmtF(sat.QualifyThreshold))
		}
		s.put(Node{Key: nodeKey(sec, "qualifies"), Bool: ptr(q), Step: "sector_qualify", Formula: f,
			Inputs: NodeInputs{Nodes: []string{nodeKey(sec, "composite"), nodeKey(sec, "veto")}, Params: map[string]float64{"threshold": sat.QualifyThreshold}}})
	}
	held := s.In.Params.SectorsHeld
	seen := map[string]bool{}
	for _, h := range held {
		switch {
		case !slices.Contains(sat.Sectors, h):
			return heldErr("unknown_sector", "Sector "+h+" is not in the satellite", map[string]any{"sector": h})
		case seen[h]:
			return heldErr("duplicate", "Sector "+h+" is listed twice", map[string]any{"sector": h})
		case !s.flag(nodeKey(h, "qualifies")):
			return heldErr("not_qualifying", "Sector "+h+" does not qualify", map[string]any{"sector": h})
		}
		seen[h] = true
	}
	if len(held) > sat.MaxSectors {
		return heldErr("too_many", fmt.Sprintf("At most %d sectors may be held", sat.MaxSectors),
			map[string]any{"max": sat.MaxSectors, "got": len(held)})
	}
	for _, sec := range sat.Sectors {
		s.put(Node{Key: nodeKey(sec, "held"), Bool: ptr(seen[sec]), Step: "sector_qualify", Formula: "analyst choice (params.sectors_held)"})
	}
	return nil
}

func heldErr(reason, msg string, meta map[string]any) error {
	return apperr.New(apperr.InvalidParams, msg, apperr.Detail{Field: "sectors_held", Reason: reason, Meta: meta})
}

// ---- constraints -------------------------------------------------------------

type constraintsStep struct{ reg Registry }

func (constraintsStep) Name() string { return "constraints" }

// Run applies the config's constraints in order.
func (c constraintsStep) Run(ctx context.Context, s *State) error {
	for _, ref := range s.In.Config.Constraints {
		con, _ := c.reg.Constraint(ref.Name)
		if err := con.Apply(ctx, s, ref); err != nil {
			return err
		}
	}
	return nil
}

// ---- normalize ---------------------------------------------------------------

type normalizeStep struct{}

func (normalizeStep) Name() string { return "normalize" }

// Run writes each bucket's preliminary p_b = n_b(1 + t_b) and its final
// within-group weight: proportional f_b = p_b / Σp, or zero_sum
// f_b = n_b(1 + t_b − Σ n_j t_j), which keeps the neutral-weighted tilt at 0.
func (normalizeStep) Run(_ context.Context, s *State) error {
	c := s.In.Config
	for _, name := range scorecardOrder(c) {
		sc := c.Scorecards[name]
		n, err := neutrals(s.In.Profile, name, sc.Buckets)
		if err != nil {
			return err
		}
		t := make([]float64, len(sc.Buckets))
		var sumP, sumNT float64
		for i, b := range sc.Buckets {
			if t[i], err = s.mustNum(nodeKey(b, "tilt"), "normalize"); err != nil {
				return err
			}
			sumP += n[i] * (1 + t[i])
			sumNT += n[i] * t[i]
		}
		for i, b := range sc.Buckets {
			p := n[i] * (1 + t[i])
			s.put(Node{Key: nodeKey(b, "prelim"), Num: ptr(p), Step: "normalize",
				Formula: fmt.Sprintf("%s × (1 + %s) = %s", fmtF(n[i]), fmtF(t[i]), fmtF(p)),
				Inputs:  NodeInputs{Nodes: []string{nodeKey(b, "tilt")}, Params: map[string]float64{"neutral": n[i]}}})
			f := p / sumP
			formula := fmt.Sprintf("%s / %s = %s", fmtF(p), fmtF(sumP), fmtF(f))
			if c.Normalisation == NormZeroSum {
				f = n[i] * (1 + t[i] - sumNT)
				formula = fmt.Sprintf("%s × (1 + %s − %s) = %s", fmtF(n[i]), fmtF(t[i]), fmtF(sumNT), fmtF(f))
			}
			s.put(Node{Key: nodeKey(b, "weight"), Num: ptr(f), Step: "normalize", Formula: formula,
				Inputs: NodeInputs{Nodes: []string{nodeKey(b, "prelim")}}})
		}
	}
	return nil
}

// neutrals reads the profile's neutral weights for a scorecard's buckets;
// they must exist and sum to 1.
func neutrals(p RiskProfile, scorecard string, buckets []string) ([]float64, error) {
	out := make([]float64, len(buckets))
	sum := decimal.Zero
	for i, b := range buckets {
		w, ok := p.NeutralWeights[b]
		if !ok {
			return nil, apperr.New(apperr.InvalidParams, "Risk profile "+p.Key+" has no neutral weight for "+b,
				apperr.Detail{Path: "/neutral_weights/" + b, Reason: "missing"})
		}
		out[i] = w.InexactFloat64()
		sum = sum.Add(w)
	}
	if !sum.Equal(decimal.NewFromInt(1)) {
		return nil, apperr.New(apperr.InvalidParams, fmt.Sprintf("Neutral weights for %s sum to %s, not 1", scorecard, sum),
			apperr.Detail{Path: "/neutral_weights", Reason: "weights_sum", Meta: map[string]any{"scorecard": scorecard, "got": sum.String()}})
	}
	return out, nil
}

// ---- sleeve ------------------------------------------------------------------

type sleeveStep struct{}

func (sleeveStep) Name() string { return "sleeve" }

// Run carves the sector sleeve out of its parent: sleeve = cap × held / max,
// split equally across held sectors; the parent's core buckets scale by
// (1 − sleeve).
func (sleeveStep) Run(_ context.Context, s *State) error {
	sat := s.In.Config.SectorSatellite
	if sat == nil {
		return nil
	}
	held := len(s.In.Params.SectorsHeld)
	sleeve := sat.SleeveCap * float64(held) / float64(sat.MaxSectors)
	s.put(Node{Key: nodeKey(sat.Parent, "sleeve"), Num: ptr(sleeve), Step: "sleeve",
		Formula: fmt.Sprintf("%s × %d / %d = %s", fmtF(sat.SleeveCap), held, sat.MaxSectors, fmtF(sleeve)),
		Inputs:  NodeInputs{Params: map[string]float64{"sleeve_cap": sat.SleeveCap, "held": float64(held), "max_sectors": float64(sat.MaxSectors)}}})
	for _, sec := range s.In.Params.SectorsHeld {
		w := sleeve / float64(held)
		s.put(Node{Key: nodeKey(sec, "within"), Num: ptr(w), Step: "sleeve",
			Formula: fmt.Sprintf("%s / %d = %s", fmtF(sleeve), held, fmtF(w)), Inputs: NodeInputs{Nodes: []string{nodeKey(sat.Parent, "sleeve")}}})
	}
	for _, name := range scorecardOrder(s.In.Config) {
		sc := s.In.Config.Scorecards[name]
		if sc.Parent != sat.Parent {
			continue
		}
		for _, b := range sc.Buckets {
			f, err := s.mustNum(nodeKey(b, "weight"), "sleeve")
			if err != nil {
				return err
			}
			w := f * (1 - sleeve)
			s.put(Node{Key: nodeKey(b, "within"), Num: ptr(w), Step: "sleeve",
				Formula: fmt.Sprintf("%s × (1 − %s) = %s", fmtF(f), fmtF(sleeve), fmtF(w)),
				Inputs:  NodeInputs{Nodes: []string{nodeKey(b, "weight"), nodeKey(sat.Parent, "sleeve")}}})
		}
	}
	return nil
}

// ---- rollup ------------------------------------------------------------------

type rollupStep struct{}

func (rollupStep) Name() string { return "rollup" }

// Run multiplies the levels into allocation lines: portfolio weight = group
// weight × within-group weight. Weights are rounded to 6 decimals by largest
// remainder, so each group's within-group weights and the whole portfolio
// sum to exactly 1.
func (rollupStep) Run(_ context.Context, s *State) error {
	c := s.In.Config
	rootName, root := rootScorecard(c)
	gw := make([]float64, len(root.Buckets))
	for i, g := range root.Buckets {
		var err error
		if gw[i], err = s.mustNum(nodeKey(g, "weight"), "rollup"); err != nil {
			return err
		}
	}
	groupDec, err := apportion(gw, "group weights of "+rootName)
	if err != nil {
		return err
	}
	var lines []AllocationLine
	var portfolio, lineG, lineW []float64
	for gi, g := range root.Buckets {
		var items []string
		var within []float64
		sleeve := map[string]bool{}
		for _, name := range scorecardOrder(c) {
			sc := c.Scorecards[name]
			if sc.Parent != g {
				continue
			}
			for _, b := range sc.Buckets {
				w, ok := s.num(nodeKey(b, "within"))
				if !ok {
					if w, err = s.mustNum(nodeKey(b, "weight"), "rollup"); err != nil {
						return err
					}
				}
				items, within = append(items, b), append(within, w)
			}
		}
		if sat := c.SectorSatellite; sat != nil && sat.Parent == g {
			for _, sec := range sat.Sectors {
				if w, ok := s.num(nodeKey(sec, "within")); ok {
					items, within = append(items, sec), append(within, w)
					sleeve[sec] = true
				}
			}
		}
		if len(items) == 0 { // a group with no level-2 split is one line
			items, within = []string{g}, []float64{1}
		}
		withinDec, err := apportion(within, "within-group weights of "+g)
		if err != nil {
			return err
		}
		ng := s.In.Profile.NeutralWeights[g]
		for i, it := range items {
			nb := s.In.Profile.NeutralWeights[it]
			if it == g {
				nb = decimal.NewFromInt(1)
			}
			lines = append(lines, AllocationLine{
				LineItem: it, Group: g, GroupWeight: groupDec[gi], WithinGroup: withinDec[i],
				NeutralWeight: ng.Mul(nb).Round(weightPlaces), Veto: s.flag(nodeKey(it, "veto")), Sleeve: sleeve[it],
			})
			portfolio = append(portfolio, gw[gi]*within[i])
			lineG, lineW = append(lineG, gw[gi]), append(lineW, within[i])
		}
	}
	portDec, err := apportion(portfolio, "portfolio weights")
	if err != nil {
		return err
	}
	for i := range lines {
		lines[i].PortfolioWeight = portDec[i]
		l := lines[i]
		s.put(Node{Key: nodeKey(l.LineItem, "portfolio"), Num: ptr(portfolio[i]), Step: "rollup",
			Formula: fmt.Sprintf("%s × %s = %s", fmtF(lineG[i]), fmtF(lineW[i]), fmtF(portfolio[i])),
			Inputs:  NodeInputs{Nodes: []string{nodeKey(l.Group, "weight"), withinNode(s, l.LineItem)}}})
	}
	s.Lines = lines
	return nil
}

func withinNode(s *State, item string) string {
	if _, ok := s.Nodes[nodeKey(item, "within")]; ok {
		return nodeKey(item, "within")
	}
	return nodeKey(item, "weight")
}

// weightPlaces is the stored precision of allocation weights (numeric(9,6)).
const weightPlaces = 6

// apportion rounds weights that sum to 1 to weightPlaces decimals by largest
// remainder, so the rounded weights sum to exactly 1.
func apportion(ws []float64, what string) ([]decimal.Decimal, error) {
	var sum float64
	for _, w := range ws {
		if w < 0 || math.IsNaN(w) || math.IsInf(w, 0) {
			return nil, apperr.New(apperr.InvalidParams, fmt.Sprintf("%s include an invalid weight %v", what, w))
		}
		sum += w
	}
	if math.Abs(sum-1) > 1e-9 {
		return nil, apperr.New(apperr.InvalidParams, fmt.Sprintf("%s sum to %v, not 1", what, sum),
			apperr.Detail{Reason: "weights_sum", Meta: map[string]any{"got": sum}})
	}
	const unit = 1e6
	out := make([]decimal.Decimal, len(ws))
	floor := make([]int64, len(ws))
	var total int64
	for i, w := range ws {
		floor[i] = int64(math.Floor(w * unit))
		total += floor[i]
	}
	idx := make([]int, len(ws))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int {
		ra, rb := ws[a]*unit-float64(floor[a]), ws[b]*unit-float64(floor[b])
		switch {
		case ra > rb:
			return -1
		case ra < rb:
			return 1
		}
		return 0
	})
	for k := 0; total < unit && k < len(idx); k++ {
		floor[idx[k]]++
		total++
	}
	for i := range ws {
		out[i] = decimal.New(floor[i], -weightPlaces)
	}
	return out, nil
}
