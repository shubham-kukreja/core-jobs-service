// Package golden loads the workbook fixtures in testdata/sheet_v2 and
// compares engine output with their expected values (gate G3).
package golden

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
)

// Scenario is one fixture file. See testdata/sheet_v2/README.md.
type Scenario struct {
	Name        string                     `json:"name"`
	Description string                     `json:"description"`
	Source      string                     `json:"source"`
	AsOf        string                     `json:"as_of"`
	TolerancePP float64                    `json:"tolerance_pp"`
	Neutral     map[string]float64         `json:"neutral"`
	Scorecards  map[string]ScorecardSpec   `json:"scorecards"`
	Inputs      Inputs                     `json:"inputs"`
	Params      map[string]json.RawMessage `json:"params"`
	Expected    map[string]float64         `json:"expected"`
}

// ScorecardSpec is the part of a scorecard a scenario needs.
type ScorecardSpec struct {
	TiltCap    float64  `json:"tilt_cap"`
	Buckets    []string `json:"buckets"`
	SleeveCap  float64  `json:"sleeve_cap"`
	MaxSectors int      `json:"max_sectors"`
}

// Inputs are a scenario's starting values.
type Inputs struct {
	Composite             map[string]float64 `json:"composite"`
	Veto                  map[string]bool    `json:"veto"`
	WithinGroupNormalized map[string]float64 `json:"within_group_normalized"`
}

// Load reads every *.golden.json in dir, sorted by file name.
func Load(dir string) ([]Scenario, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.golden.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	out := make([]Scenario, 0, len(paths))
	for _, p := range paths {
		b, err := os.ReadFile(p) //nolint:gosec // fixture paths come from the repo's testdata.
		if err != nil {
			return nil, err
		}
		var s Scenario
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		if s.Name == "" || len(s.Expected) == 0 || s.TolerancePP <= 0 {
			return nil, fmt.Errorf("%s: name, expected and tolerance_pp are required", filepath.Base(p))
		}
		out = append(out, s)
	}
	return out, nil
}

// Mismatch is one expected value the actual output missed.
type Mismatch struct {
	Key      string
	Expected float64
	Actual   float64
	Missing  bool
}

func (m Mismatch) String() string {
	if m.Missing {
		return fmt.Sprintf("%s: missing (want %.6f)", m.Key, m.Expected)
	}
	return fmt.Sprintf("%s: got %.6f, want %.6f (off by %.3fpp)", m.Key, m.Actual, m.Expected, math.Abs(m.Actual-m.Expected)*100)
}

// Compare checks actual against s.Expected within s.TolerancePP. Weights and
// tilts are fractions, so the tolerance is applied as pp/100.
func Compare(s Scenario, actual map[string]float64) []Mismatch {
	tol := s.TolerancePP/100 + 1e-9
	keys := make([]string, 0, len(s.Expected))
	for k := range s.Expected {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []Mismatch
	for _, k := range keys {
		want := s.Expected[k]
		got, ok := actual[k]
		switch {
		case !ok:
			out = append(out, Mismatch{Key: k, Expected: want, Missing: true})
		case math.Abs(got-want) > tol:
			out = append(out, Mismatch{Key: k, Expected: want, Actual: got})
		}
	}
	return out
}
