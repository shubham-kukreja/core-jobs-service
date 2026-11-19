package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
)

// The built-in constraints are the workbook's standing rules (Monitoring &
// Review). They run in the constraints step, after tilt and before normalize.

func decodeStrict(p json.RawMessage, v any) error {
	if len(p) == 0 {
		p = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(p))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func paramErr(name, msg string) error {
	return apperr.New(apperr.InvalidParams, name+": "+msg, apperr.Detail{Reason: "invalid_params", Meta: map[string]any{"constraint": name}})
}

// targets lists the (scorecard, bucket) pairs a ref applies to: one bucket,
// one scorecard's buckets, or every scorecard's.
func targets(c Config, ref ConstraintRef) [][2]string {
	var out [][2]string
	for _, name := range scorecardOrder(c) {
		if ref.Scorecard != "" && name != ref.Scorecard {
			continue
		}
		for _, b := range c.Scorecards[name].Buckets {
			if ref.Bucket == "" || b == ref.Bucket {
				out = append(out, [2]string{name, b})
			}
		}
	}
	return out
}

// ---- max_multiple_of_neutral -------------------------------------------------

// maxMultipleOfNeutral keeps a bucket's preliminary weight at most max ×
// neutral ("precious metals: never > 2× neutral") by capping its tilt at
// max − 1.
type maxMultipleOfNeutral struct{}

type maxMultipleParams struct {
	Max float64 `json:"max"`
}

func (maxMultipleOfNeutral) Name() string { return "max_multiple_of_neutral" }

func (m maxMultipleOfNeutral) ValidateParams(p json.RawMessage) error {
	var x maxMultipleParams
	if err := decodeStrict(p, &x); err != nil {
		return paramErr(m.Name(), err.Error())
	}
	if x.Max <= 1 || x.Max > 100 {
		return paramErr(m.Name(), "max must be in (1, 100]")
	}
	return nil
}

func (m maxMultipleOfNeutral) Apply(_ context.Context, s *State, ref ConstraintRef) error {
	var x maxMultipleParams
	if err := decodeStrict(ref.Params, &x); err != nil {
		return paramErr(m.Name(), err.Error())
	}
	limit := x.Max - 1
	for _, tb := range targets(s.In.Config, ref) {
		key := nodeKey(tb[1], "tilt")
		t, err := s.mustNum(key, m.Name())
		if err != nil {
			return err
		}
		if t <= limit {
			continue
		}
		n := s.Nodes[key]
		n.Num = ptr(limit)
		n.Formula += fmt.Sprintf("; capped at %s (weight ≤ %s × neutral)", fmtF(limit), fmtF(x.Max))
		n.Inputs.Params["max_multiple"] = x.Max
		s.Nodes[key] = n
	}
	return nil
}

// ---- extreme_full_cap ----------------------------------------------------------

// extremeFullCap allows the full tilt cap only to buckets whose composite has
// been extreme (> high or < low) for consecutive_reviews reviews, counting
// this one; others tilt within cap × normal_cap_factor. A factor of 1 leaves
// every tilt unchanged (the rule's exact form is an open question).
type extremeFullCap struct{}

type extremeParams struct {
	High               float64 `json:"high"`
	Low                float64 `json:"low"`
	ConsecutiveReviews int     `json:"consecutive_reviews"`
	NormalCapFactor    float64 `json:"normal_cap_factor"`
}

func (extremeFullCap) Name() string { return "extreme_full_cap" }

func (e extremeFullCap) ValidateParams(p json.RawMessage) error {
	var x extremeParams
	if err := decodeStrict(p, &x); err != nil {
		return paramErr(e.Name(), err.Error())
	}
	switch {
	case x.Low < 0 || x.High > 100 || x.Low >= x.High:
		return paramErr(e.Name(), "need 0 <= low < high <= 100")
	case x.ConsecutiveReviews < 1 || x.ConsecutiveReviews > 20:
		return paramErr(e.Name(), "consecutive_reviews must be 1..20")
	case x.NormalCapFactor <= 0 || x.NormalCapFactor > 1:
		return paramErr(e.Name(), "normal_cap_factor must be in (0, 1]")
	}
	return nil
}

func (e extremeFullCap) Apply(_ context.Context, s *State, ref ConstraintRef) error {
	var x extremeParams
	if err := decodeStrict(ref.Params, &x); err != nil {
		return paramErr(e.Name(), err.Error())
	}
	for _, tb := range targets(s.In.Config, ref) {
		sc, b := s.In.Config.Scorecards[tb[0]], tb[1]
		comp, err := s.mustNum(nodeKey(b, "composite"), e.Name())
		if err != nil {
			return err
		}
		extreme := comp > x.High || comp < x.Low
		runs := 0
		if extreme {
			runs = s.In.History.ExtremeRuns[b] + 1
		}
		full := extreme && runs >= x.ConsecutiveReviews
		f := fmt.Sprintf("%s within [%s, %s]", fmtF(comp), fmtF(x.Low), fmtF(x.High))
		if extreme {
			f = fmt.Sprintf("%s outside [%s, %s] for %d of %d reviews", fmtF(comp), fmtF(x.Low), fmtF(x.High), runs, x.ConsecutiveReviews)
		}
		s.put(Node{Key: nodeKey(b, "extreme"), Bool: ptr(full), Step: "constraints", Formula: f,
			Inputs: NodeInputs{Nodes: []string{nodeKey(b, "composite")}, Params: map[string]float64{"runs": float64(runs)}}})
		if full || x.NormalCapFactor == 1 {
			continue
		}
		limit := sc.TiltCap * x.NormalCapFactor
		if math.Abs(limit-sc.TiltCap) < 1e-15 {
			continue
		}
		s.putTilt(b, comp, limit, fmt.Sprintf("normal cap %s × %s (not extreme long enough for the full cap)", fmtF(sc.TiltCap), fmtF(x.NormalCapFactor)))
	}
	return nil
}

// ---- sector_max_tenure -------------------------------------------------------

// sectorMaxTenure fails a run that holds a sector for more than `quarters`
// consecutive quarters ("a sector exits after 4 consecutive quarters in the
// sleeve regardless of score").
type sectorMaxTenure struct{}

type tenureParams struct {
	Quarters int `json:"quarters"`
}

func (sectorMaxTenure) Name() string { return "sector_max_tenure" }

func (t sectorMaxTenure) ValidateParams(p json.RawMessage) error {
	var x tenureParams
	if err := decodeStrict(p, &x); err != nil {
		return paramErr(t.Name(), err.Error())
	}
	if x.Quarters < 1 || x.Quarters > 40 {
		return paramErr(t.Name(), "quarters must be 1..40")
	}
	return nil
}

func (t sectorMaxTenure) Apply(_ context.Context, s *State, ref ConstraintRef) error {
	var x tenureParams
	if err := decodeStrict(ref.Params, &x); err != nil {
		return paramErr(t.Name(), err.Error())
	}
	for _, sec := range s.In.Params.SectorsHeld {
		if q := s.In.History.QuartersHeld[sec]; q >= x.Quarters {
			return heldErr("max_tenure", fmt.Sprintf("Sector %s has been held %d consecutive quarters; it must exit (max %d)", sec, q, x.Quarters),
				map[string]any{"sector": sec, "quarters_held": q, "max": x.Quarters})
		}
	}
	return nil
}
