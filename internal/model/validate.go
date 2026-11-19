package model

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
)

// weightTolerance is how far signal weights may sum from 1.
const weightTolerance = 1e-9

type problems []apperr.Detail

func (p *problems) add(path, reason string, meta map[string]any) {
	*p = append(*p, apperr.Detail{Path: path, Reason: reason, Meta: meta})
}

func (p problems) err() error {
	if len(p) == 0 {
		return nil
	}
	return apperr.New(apperr.ValidationFailed, "Invalid model config", p...)
}

// CheckConfig enforces the semantic rules the JSON Schema cannot: registered
// steps and constraints, one root scorecard, parents that exist, unique
// buckets, signal weights that sum to 1, valid templates and veto keys.
// Errors are validation_failed with a JSON pointer per problem.
func CheckConfig(c Config, reg Registry) error {
	var p problems
	if len(c.Steps) == 0 {
		p.add("/steps", "required", nil)
	}
	seenStep := map[string]bool{}
	for i, name := range c.Steps {
		path := "/steps/" + strconv.Itoa(i)
		if _, ok := reg.Step(name); !ok {
			p.add(path, "unknown_step", map[string]any{"step": name})
		}
		if seenStep[name] {
			p.add(path, "duplicate", map[string]any{"step": name})
		}
		seenStep[name] = true
	}
	if c.Normalisation != NormProportional && c.Normalisation != NormZeroSum {
		p.add("/normalisation", "invalid_enum", map[string]any{"allowed": []string{NormProportional, NormZeroSum}})
	}
	if c.MissingScore < 0 || c.MissingScore > 100 || math.IsNaN(c.MissingScore) {
		p.add("/missing_score", "out_of_range", map[string]any{"min": 0, "max": 100})
	}

	var roots []string
	for name, sc := range c.Scorecards {
		if sc.Parent == "" {
			roots = append(roots, name)
		}
	}
	sort.Strings(roots)
	if len(roots) != 1 {
		p.add("/scorecards", "one_root", map[string]any{"roots": roots})
	}
	rootBuckets := map[string]bool{}
	if len(roots) == 1 {
		for _, b := range c.Scorecards[roots[0]].Buckets {
			rootBuckets[b] = true
		}
	}
	owner := map[string]string{} // bucket → where it is defined
	claim := func(path, b string) {
		if prev, ok := owner[b]; ok {
			p.add(path, "duplicate_bucket", map[string]any{"bucket": b, "also": prev})
			return
		}
		owner[b] = path
	}
	names := make([]string, 0, len(c.Scorecards))
	for name := range c.Scorecards {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sc := c.Scorecards[name]
		base := "/scorecards/" + name
		if len(sc.Buckets) == 0 {
			p.add(base+"/buckets", "required", nil)
		}
		for i, b := range sc.Buckets {
			claim(fmt.Sprintf("%s/buckets/%d", base, i), b)
		}
		if sc.TiltCap < 0 || sc.TiltCap >= 1 {
			p.add(base+"/tilt_cap", "out_of_range", map[string]any{"min": 0, "max_exclusive": 1})
		}
		if sc.Parent != "" && !rootBuckets[sc.Parent] {
			p.add(base+"/parent", "unknown_parent", map[string]any{"parent": sc.Parent})
		}
		checkSignals(&p, base, sc.Signals)
		checkTemplate(&p, base+"/score_series", sc.ScoreSeries)
		checkVetoes(&p, base+"/vetoes", sc.Vetoes, sc.Buckets)
	}
	if sat := c.SectorSatellite; sat != nil {
		base := "/sector_satellite"
		if !rootBuckets[sat.Parent] {
			p.add(base+"/parent", "unknown_parent", map[string]any{"parent": sat.Parent})
		}
		if len(sat.Sectors) == 0 {
			p.add(base+"/sectors", "required", nil)
		}
		for i, sec := range sat.Sectors {
			claim(fmt.Sprintf("%s/sectors/%d", base, i), sec)
		}
		checkSignals(&p, base, sat.Signals)
		checkTemplate(&p, base+"/score_series", sat.ScoreSeries)
		checkVetoes(&p, base+"/vetoes", sat.Vetoes, sat.Sectors)
		if sat.SleeveCap < 0 || sat.SleeveCap > 1 {
			p.add(base+"/sleeve_cap", "out_of_range", map[string]any{"min": 0, "max": 1})
		}
		if sat.MaxSectors < 1 {
			p.add(base+"/max_sectors", "out_of_range", map[string]any{"min": 1})
		}
		if sat.QualifyThreshold < 0 || sat.QualifyThreshold > 100 {
			p.add(base+"/qualify_threshold", "out_of_range", map[string]any{"min": 0, "max": 100})
		}
	}
	for i, ref := range c.Constraints {
		base := "/constraints/" + strconv.Itoa(i)
		con, ok := reg.Constraint(ref.Name)
		if !ok {
			p.add(base+"/name", "unknown_constraint", map[string]any{"constraint": ref.Name})
			continue
		}
		if err := con.ValidateParams(ref.Params); err != nil {
			p.add(base+"/params", "invalid_params", map[string]any{"error": err.Error()})
		}
		if ref.Scorecard != "" {
			sc, ok := c.Scorecards[ref.Scorecard]
			switch {
			case !ok:
				p.add(base+"/scorecard", "unknown_scorecard", map[string]any{"scorecard": ref.Scorecard})
			case ref.Bucket != "" && !contains(sc.Buckets, ref.Bucket):
				p.add(base+"/bucket", "unknown_bucket", map[string]any{"bucket": ref.Bucket})
			}
		} else if ref.Bucket != "" {
			p.add(base+"/scorecard", "required", map[string]any{"why": "bucket needs its scorecard"})
		}
	}
	return p.err()
}

func checkSignals(p *problems, base string, sigs []Signal) {
	if len(sigs) == 0 {
		p.add(base+"/signals", "required", nil)
		return
	}
	var sum float64
	seen := map[string]bool{}
	for i, s := range sigs {
		if s.Weight < 0 || s.Weight > 1 {
			p.add(fmt.Sprintf("%s/signals/%d/weight", base, i), "out_of_range", map[string]any{"min": 0, "max": 1})
		}
		if seen[s.Key] {
			p.add(fmt.Sprintf("%s/signals/%d/key", base, i), "duplicate", map[string]any{"signal": s.Key})
		}
		seen[s.Key] = true
		sum += s.Weight
	}
	if math.Abs(sum-1) > weightTolerance {
		p.add(base+"/signals", "weights_sum", map[string]any{"got": sum, "want": 1})
	}
}

func checkTemplate(p *problems, path, tmpl string) {
	if !strings.Contains(tmpl, "{bucket}") || !strings.Contains(tmpl, "{signal}") {
		p.add(path, "template", map[string]any{"need": []string{"{bucket}", "{signal}"}})
	}
}

func checkVetoes(p *problems, base string, vetoes map[string][]string, buckets []string) {
	keys := make([]string, 0, len(vetoes))
	for k := range vetoes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k != "*" && !contains(buckets, k) {
			p.add(base+"/"+k, "unknown_bucket", map[string]any{"bucket": k})
		}
		for i, t := range vetoes[k] {
			if strings.Contains(t, "{signal}") {
				p.add(fmt.Sprintf("%s/%s/%d", base, k, i), "template", map[string]any{"why": "veto templates take {bucket} only"})
			}
		}
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// ReferencedSeries lists every score and trigger series a config reads, with
// the JSON pointer of the template that names it.
func ReferencedSeries(c Config) map[string]string {
	out := map[string]string{}
	add := func(key, path string) {
		if _, ok := out[key]; !ok {
			out[key] = path
		}
	}
	for _, name := range scorecardOrder(c) {
		sc, ok := c.Scorecards[name]
		if !ok {
			continue
		}
		base := "/scorecards/" + name
		for _, b := range sc.Buckets {
			for _, sg := range sc.Signals {
				add(expand(sc.ScoreSeries, b, sg.Key), base+"/score_series")
			}
			for _, k := range []string{"*", b} {
				for i, t := range sc.Vetoes[k] {
					add(expand(t, b, ""), fmt.Sprintf("%s/vetoes/%s/%d", base, k, i))
				}
			}
		}
	}
	if sat := c.SectorSatellite; sat != nil {
		for _, sec := range sat.Sectors {
			for _, sg := range sat.Signals {
				add(expand(sat.ScoreSeries, sec, sg.Key), "/sector_satellite/score_series")
			}
			for _, k := range []string{"*", sec} {
				for i, t := range sat.Vetoes[k] {
					add(expand(t, sec, ""), fmt.Sprintf("/sector_satellite/vetoes/%s/%d", k, i))
				}
			}
		}
	}
	return out
}

// SeriesLister reads series definitions by key.
type SeriesLister interface {
	ListSeries(ctx context.Context, f catalog.SeriesFilter) (paging.Page[catalog.Series], error)
}

type validator struct {
	reg    Registry
	series SeriesLister
}

// NewValidator returns the config Validator: CheckConfig plus every
// referenced score and trigger series must exist in the catalog.
func NewValidator(reg Registry, series SeriesLister) Validator {
	return validator{reg: reg, series: series}
}

func (v validator) Validate(ctx context.Context, c Config) error {
	if err := CheckConfig(c, v.reg); err != nil {
		return err
	}
	refs := ReferencedSeries(c)
	keys := make([]string, 0, len(refs))
	for k := range refs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	found := map[string]bool{}
	for start := 0; start < len(keys); start += paging.MaxLimit {
		batch := keys[start:min(start+paging.MaxLimit, len(keys))]
		req := paging.Request{Limit: paging.MaxLimit}
		for {
			page, err := v.series.ListSeries(ctx, catalog.SeriesFilter{Keys: batch, Page: req})
			if err != nil {
				return err
			}
			for _, s := range page.Items {
				found[s.Key] = true
			}
			if page.Next == "" {
				break
			}
			req.After = page.Next
		}
	}
	var p problems
	for _, k := range keys {
		if !found[k] {
			p.add(refs[k], "unknown_series", map[string]any{"series": k})
		}
	}
	return p.err()
}
