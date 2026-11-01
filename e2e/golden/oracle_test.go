package golden

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// oracle recomputes a scenario's expected values directly from the plan's
// formulas (Model engine > Formulas). It is deliberately independent of the
// model package, so a mistyped fixture fails here rather than in the engine.
func oracle(s Scenario) map[string]float64 {
	out := map[string]float64{}
	for _, sc := range s.Scorecards {
		if sc.TiltCap == 0 {
			continue
		}
		prelim := map[string]float64{}
		sum := 0.0
		for _, b := range sc.Buckets {
			c, ok := s.Inputs.Composite[b]
			if !ok {
				continue
			}
			t := (c - 50) / 50 * sc.TiltCap
			if s.Inputs.Veto[b] && t > 0 {
				t = 0
			}
			out["tilt."+b] = t
			if n, ok := s.Neutral[b]; ok {
				prelim[b] = n * (1 + t)
				sum += prelim[b]
			}
		}
		for b, p := range prelim {
			out["preliminary."+b] = p
			out["final."+b] = p / sum
		}
	}
	if sat, ok := s.Scorecards["sector_satellite"]; ok {
		var held []string
		_ = json.Unmarshal(s.Params["sectors_held"], &held)
		sleeve := sat.SleeveCap * float64(len(held)) / float64(sat.MaxSectors)
		out["sleeve.l1.equity"] = sleeve
		for b, f := range s.Inputs.WithinGroupNormalized {
			out["core."+b] = f * (1 - sleeve)
		}
	}
	return out
}

func TestFixturesAreConsistent(t *testing.T) {
	scenarios, err := Load(filepath.Join("..", "..", "testdata", "sheet_v2"))
	if err != nil {
		t.Fatal(err)
	}
	if len(scenarios) == 0 {
		t.Fatal("no fixtures found")
	}
	for _, s := range scenarios {
		t.Run(s.Name, func(t *testing.T) {
			for _, m := range Compare(s, oracle(s)) {
				t.Error(m)
			}
		})
	}
}

func TestCompareReportsMismatches(t *testing.T) {
	s := Scenario{TolerancePP: 0.05, Expected: map[string]float64{"a": 0.5, "b": 0.25}}
	got := Compare(s, map[string]float64{"a": 0.5004})
	if len(got) != 1 || got[0].Key != "b" || !got[0].Missing {
		t.Fatalf("within tolerance must pass and missing must fail, got %v", got)
	}
	got = Compare(s, map[string]float64{"a": 0.501, "b": 0.25})
	if len(got) != 1 || got[0].Key != "a" {
		t.Fatalf("0.1pp off must fail, got %v", got)
	}
}
