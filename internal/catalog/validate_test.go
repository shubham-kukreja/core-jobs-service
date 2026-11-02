package catalog

import (
	"math"
	"testing"
	"time"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
)

func f(v float64) *float64 { return &v }
func i(v int) *int         { return &v }

func validRaw() SeriesInput {
	return SeriesInput{Key: "pe.nifty100", Name: "Nifty 100 P/E", Layer: LayerRaw, ValueType: ValueNum, Frequency: FreqMonthly,
		Validation: Validation{Min: f(0), Max: f(200)}}
}

func TestNormalizeSeriesAcceptsValid(t *testing.T) {
	in := validRaw()
	if err := normalizeSeries(&in); err != nil {
		t.Fatal(err)
	}
	if in.Align != AlignLast {
		t.Fatalf("align default not applied: %q", in.Align)
	}
	score := SeriesInput{Key: "l1.equity::macro", Name: "Equity - Macro", Layer: LayerScore, ValueType: ValueNum, Frequency: FreqMonthly,
		Derivation: &Derivation{Fn: "rubric", Inputs: map[string]string{"stance": "judg.rbi.stance"}},
		Registry:   RegistryMeta{NodeKey: "l1.equity", SignalKey: "macro", Group: "equity", UpdateType: "rubric", StaleAfter: 40 * 24 * time.Hour}}
	if err := normalizeSeries(&score); err != nil {
		t.Fatal(err)
	}
	cat := SeriesInput{Key: "judg.rbi.stance", Name: "RBI stance", Layer: LayerJudgment, ValueType: ValueCat, Frequency: FreqMonthly,
		AllowedValues: []string{"cutting", "hold", "hiking"}}
	if err := normalizeSeries(&cat); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeSeriesRejects(t *testing.T) {
	cases := map[string]struct {
		mutate func(*SeriesInput)
		reason string
	}{
		"bad key":            {func(s *SeriesInput) { s.Key = "PE Nifty" }, "invalid_key"},
		"empty name":         {func(s *SeriesInput) { s.Name = "" }, "invalid_length"},
		"unknown layer":      {func(s *SeriesInput) { s.Layer = "magic" }, "unknown_layer"},
		"min above max":      {func(s *SeriesInput) { s.Validation = Validation{Min: f(5), Max: f(1)} }, "min_above_max"},
		"nan bound":          {func(s *SeriesInput) { s.Validation = Validation{Min: f(math.NaN())} }, "not_finite"},
		"raw with formula":   {func(s *SeriesInput) { s.Derivation = &Derivation{Fn: "expr"} }, "not_allowed_for_layer"},
		"score w/o formula":  {func(s *SeriesInput) { s.Layer = LayerScore }, "required_for_layer"},
		"trigger num":        {func(s *SeriesInput) { s.Layer = LayerTrigger; s.Validation = Validation{} }, "layer_requires_bool"},
		"cat w/o values":     {func(s *SeriesInput) { s.ValueType = ValueCat; s.Validation = Validation{} }, "required_for_cat"},
		"values on num":      {func(s *SeriesInput) { s.AllowedValues = []string{"a", "b"} }, "only_for_cat"},
		"bounds on bool":     {func(s *SeriesInput) { s.ValueType = ValueBool }, "only_for_num"},
		"min history":        {func(s *SeriesInput) { s.MinHistory = i(0) }, "must_be_positive"},
		"duplicate priority": {func(s *SeriesInput) { s.SourcePriority = []string{"analyst", "analyst"} }, "invalid_or_duplicate"},
		"bad tag":            {func(s *SeriesInput) { s.Tags = []string{"Has Space"} }, "invalid_tag"},
		"bad group":          {func(s *SeriesInput) { s.Registry.Group = "crypto" }, "unknown_group"},
		"bad flag":           {func(s *SeriesInput) { s.Registry.Flags = []string{"hidden"} }, "unknown_flag"},
		"negative stale":     {func(s *SeriesInput) { s.Registry.StaleAfter = -time.Hour }, "must_not_be_negative"},
		"self reference": {func(s *SeriesInput) {
			s.Layer = LayerDerived
			s.Derivation = &Derivation{Fn: "expr", Inputs: map[string]string{"x": s.Key}}
		}, "self_reference"},
		"bad alias": {func(s *SeriesInput) {
			s.Layer = LayerDerived
			s.Derivation = &Derivation{Fn: "expr", Inputs: map[string]string{"X-1": "a.b"}}
		}, "invalid_alias"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := validRaw()
			tc.mutate(&in)
			err := normalizeSeries(&in)
			e, ok := apperr.As(err)
			if !ok || e.Code != apperr.ValidationFailed {
				t.Fatalf("want validation_failed, got %v", err)
			}
			for _, d := range e.Details {
				if d.Reason == tc.reason {
					return
				}
			}
			t.Fatalf("want reason %q in %+v", tc.reason, e.Details)
		})
	}
}

func TestValidateEntity(t *testing.T) {
	if err := validateEntity(Entity{Key: "equity.large", Kind: "segment", Name: "Large Cap", ParentKey: "l1.equity"}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []Entity{
		{Key: "Equity", Kind: "segment", Name: "x"},
		{Key: "a", Kind: "planet", Name: "x"},
		{Key: "a", Kind: "segment", Name: ""},
		{Key: "a", Kind: "segment", Name: "x", ParentKey: "a"},
	} {
		if err := validateEntity(e); !apperr.Is(err, apperr.ValidationFailed) {
			t.Errorf("%+v: want validation error, got %v", e, err)
		}
	}
}
