package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/civil"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/derive"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/calendar"
)

func shipped(t *testing.T) (Documents, []catalog.SeriesInput) {
	t.Helper()
	docs, err := Validate(filepath.Join("..", "..", "configs", "seed"))
	if err != nil {
		t.Fatal(err)
	}
	series, err := SeriesInputs(docs)
	if err != nil {
		t.Fatal(err)
	}
	return docs, series
}

// TestShippedSeriesLoad runs every seeded series through the checks SaveSeries
// makes before touching the database, and every derivation through the
// primitive registry (P2-T7-03: loads with no validation errors).
func TestShippedSeriesLoad(t *testing.T) {
	_, series := shipped(t)
	reg := derive.NewDefaultRegistry()
	pos := map[string]int{}
	for i, s := range series {
		pos[s.Key] = i
	}
	for _, s := range series {
		if _, err := catalog.ValidateSeriesInput(s); err != nil {
			t.Errorf("%s: %v", s.Key, err)
		}
		if s.Derivation == nil {
			continue
		}
		if _, err := reg.ValidateDerivation(*s.Derivation); err != nil {
			t.Errorf("%s: derivation: %v", s.Key, err)
		}
		for alias, in := range s.Derivation.Inputs {
			if pos[in] >= pos[s.Key] {
				t.Errorf("%s: input %s (%s) is not ordered before it", s.Key, alias, in)
			}
		}
	}
}

// TestWorkbookCoverage pins the workbook's shape: 85 scores, one per bucket
// and signal of every scorecard, and a trigger series for every veto.
func TestWorkbookCoverage(t *testing.T) {
	docs, series := shipped(t)
	byKey := map[string]catalog.SeriesInput{}
	scores := 0
	for _, s := range series {
		byKey[s.Key] = s
		if s.Layer == catalog.LayerScore {
			scores++
		}
	}
	if scores != 85 {
		t.Errorf("score series = %d, want the workbook's 85", scores)
	}
	want := func(key string, layer catalog.Layer, where string) {
		t.Helper()
		s, ok := byKey[key]
		switch {
		case !ok:
			t.Errorf("%s: no series %q", where, key)
		case s.Layer != layer:
			t.Errorf("%s: %q is a %s series, want %s", where, key, s.Layer, layer)
		}
	}
	expand := func(tmpl, bucket, signal string) string {
		return strings.NewReplacer("{bucket}", bucket, "{signal}", signal).Replace(tmpl)
	}
	referenced := map[string]bool{}
	for path, doc := range docs {
		if !strings.HasPrefix(filepath.Base(path), "model_") {
			continue
		}
		cfg := obj(obj(doc)["config"])
		for name, sc := range obj(cfg["scorecards"]) {
			sc := obj(sc)
			buckets := anyList(sc["buckets"])
			for _, b := range buckets {
				for _, sig := range list(sc["signals"]) {
					k := expand(str(sc, "score_series"), fmt.Sprint(b), str(sig, "key"))
					want(k, catalog.LayerScore, path+": scorecards/"+name)
					referenced[k] = true
				}
			}
			for target, triggers := range obj(sc["vetoes"]) {
				applies := []any{target}
				if target == "*" {
					applies = buckets
				}
				for _, b := range applies {
					for _, tr := range anyList(triggers) {
						want(expand(fmt.Sprint(tr), fmt.Sprint(b), ""), catalog.LayerTrigger, path+": scorecards/"+name+"/vetoes")
					}
				}
			}
		}
		if sat := obj(cfg["sector_satellite"]); sat != nil {
			for _, sec := range anyList(sat["sectors"]) {
				for _, sig := range list(sat["signals"]) {
					k := expand(str(sat, "score_series"), fmt.Sprint(sec), str(sig, "key"))
					want(k, catalog.LayerScore, path+": sector_satellite")
					referenced[k] = true
				}
			}
		}
	}
	for k, s := range byKey {
		if s.Layer == catalog.LayerScore && !referenced[k] {
			t.Errorf("score %q is not used by any scorecard", k)
		}
	}
}

func TestSeriesInputsOrder(t *testing.T) {
	got, err := topoOrder(map[string]catalog.SeriesInput{
		"c": {Key: "c", Derivation: &catalog.Derivation{Inputs: map[string]string{"x": "b", "y": "a"}}},
		"b": {Key: "b", Derivation: &catalog.Derivation{Inputs: map[string]string{"x": "a"}}},
		"a": {Key: "a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, s := range got {
		keys = append(keys, s.Key)
	}
	if strings.Join(keys, ",") != "a,b,c" {
		t.Fatalf("order = %v", keys)
	}
	if _, err := parseStaleAfter("40d"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"40", "d", "-1d", "4w"} {
		if _, err := parseStaleAfter(bad); err == nil {
			t.Errorf("parseStaleAfter(%q): want error", bad)
		}
	}
}

// TestCategoryTablesCoverAllowedValues: a rubric or bucket_map that misses one
// of its judgment's allowed values fails at compute time, not at save time.
func TestCategoryTablesCoverAllowedValues(t *testing.T) {
	_, series := shipped(t)
	byKey := map[string]catalog.SeriesInput{}
	for _, s := range series {
		byKey[s.Key] = s
	}
	for _, s := range series {
		d := s.Derivation
		if d == nil || (d.Fn != "rubric" && d.Fn != "bucket_map") {
			continue
		}
		var p struct {
			Add        map[string]map[string]float64 `json:"add"`
			Categories map[string]float64            `json:"categories"`
		}
		if err := json.Unmarshal(d.Params, &p); err != nil {
			t.Fatal(err)
		}
		tables := p.Add
		if d.Fn == "bucket_map" {
			if p.Categories == nil {
				continue // numeric ranges
			}
			tables = map[string]map[string]float64{"x": p.Categories}
		}
		for alias, table := range tables {
			in := byKey[d.Inputs[alias]]
			if in.ValueType != catalog.ValueCat {
				t.Errorf("%s: %s reads %s, which is not categorical", s.Key, alias, in.Key)
				continue
			}
			for _, v := range in.AllowedValues {
				if _, ok := table[v]; !ok {
					t.Errorf("%s: %s has no entry for %s = %q", s.Key, alias, in.Key, v)
				}
			}
			for v := range table {
				if !slices.Contains(in.AllowedValues, v) {
					t.Errorf("%s: %s maps %q, which %s never takes", s.Key, alias, v, in.Key)
				}
			}
		}
	}
}

// TestEveryDerivationComputes evaluates each derivation once on 30 months of
// synthetic input history, catching runtime errors (expr type mismatches,
// unknown categories) that params validation cannot see.
func TestEveryDerivationComputes(t *testing.T) {
	_, series := shipped(t)
	byKey := map[string]catalog.SeriesInput{}
	for _, s := range series {
		byKey[s.Key] = s
	}
	reg := derive.NewDefaultRegistry()
	env := derive.Env{Governance: derive.Governance{PercentileMinObservations: 24}}
	asOf := civil.Date{Year: 2026, Month: 6, Day: 30}
	history := func(in catalog.SeriesInput) []datapoints.Resolved {
		var out []datapoints.Resolved
		for i := 29; i >= 0; i-- {
			d := calendar.MonthEnd(calendar.AddMonths(asOf, -i))
			var v datapoints.Value
			switch in.ValueType {
			case catalog.ValueCat:
				v = datapoints.Cat(in.AllowedValues[i%len(in.AllowedValues)])
			case catalog.ValueBool:
				v = datapoints.Bool(i%2 == 0)
			default:
				v = datapoints.Num(40 + float64(i%7)*3) // varied, positive, 40..58
			}
			out = append(out, datapoints.Resolved{SeriesKey: in.Key, ObsDate: d, Value: v})
		}
		return out
	}
	for _, s := range series {
		if s.Derivation == nil {
			continue
		}
		p, _ := reg.Get(s.Derivation.Fn)
		in := derive.Inputs{}
		for alias, key := range s.Derivation.Inputs {
			in[alias] = history(byKey[key])
		}
		v, err := p.Compute(context.Background(), env, asOf, in, s.Derivation.Params)
		if err != nil {
			t.Errorf("%s: %v", s.Key, err)
			continue
		}
		if v.IsNull() {
			t.Errorf("%s: null on full synthetic history", s.Key)
		}
		if s.Layer == catalog.LayerScore && (v.Num == nil || *v.Num < 0 || *v.Num > 100) {
			t.Errorf("%s: score %v outside 0..100", s.Key, v)
		}
	}
}
