package catalog

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
)

func TestSplitKeyPatterns(t *testing.T) {
	keys, pats, err := splitKeyPatterns([]string{"pe.nifty50", "score_l1.*", "l1.equity::*"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keys, []string{"pe.nifty50"}) {
		t.Fatalf("keys = %v", keys)
	}
	// '_' must be escaped or it would match any character.
	if !reflect.DeepEqual(pats, []string{`score\_l1.%`, `l1.equity::%`}) {
		t.Fatalf("patterns = %v", pats)
	}
	for _, bad := range []string{"*", "a*b*", "a*b", "Bad Key", "x%"} {
		if _, _, err := splitKeyPatterns([]string{bad}); !apperr.Is(err, apperr.ValidationFailed) {
			t.Errorf("%q: want validation error, got %v", bad, err)
		}
	}
}

func TestEscapeLike(t *testing.T) {
	if got := escapeLike(`a_b%c\d`); got != `a\_b\%c\\d` {
		t.Fatalf("escapeLike = %q", got)
	}
}

func TestSameDerivation(t *testing.T) {
	a := Derivation{Fn: "percentile_rank", Inputs: map[string]string{"x": "spread.mid_large"},
		Params: json.RawMessage(`{"window_months": 180, "min_obs": 24}`), Version: 1}
	b := a
	b.Params = json.RawMessage(`{"min_obs":24,"window_months":180}`)
	b.Version = 7
	if same, err := sameDerivation(a, b); err != nil || !same {
		t.Fatalf("formatting/version must not matter: %v %v", same, err)
	}
	b.Params = json.RawMessage(`{"min_obs":24,"window_months":120}`)
	if same, _ := sameDerivation(a, b); same {
		t.Fatal("param change not detected")
	}
	c := a
	c.Inputs = map[string]string{"x": "spread.small_large"}
	if same, _ := sameDerivation(a, c); same {
		t.Fatal("input change not detected")
	}
	d := a
	d.Params = json.RawMessage(`{bad`)
	if _, err := sameDerivation(a, d); !apperr.Is(err, apperr.ValidationFailed) {
		t.Fatalf("invalid params: %v", err)
	}
}

func TestIntervalRoundTrip(t *testing.T) {
	for _, d := range []time.Duration{0, 2 * 24 * time.Hour, 40 * 24 * time.Hour, 90 * time.Minute} {
		if got := duration(interval(d)); got != d {
			t.Errorf("round trip %v → %v", d, got)
		}
	}
}

func TestUniqueSorted(t *testing.T) {
	if got := uniqueSorted([]string{"b", "a", "b", "c", "a"}); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("uniqueSorted = %v", got)
	}
}
