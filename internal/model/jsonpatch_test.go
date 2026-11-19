package model

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
)

func op(o, path, v string) PatchOp {
	p := PatchOp{Op: o, Path: path}
	if v != "" {
		p.Value = json.RawMessage(v)
	}
	return p
}

func TestApplyPatch(t *testing.T) {
	c := seedConfig(t)
	next, err := ApplyPatch(c, []PatchOp{
		op("test", "/scorecards/debt/tilt_cap", "0.3"),
		op("replace", "/scorecards/debt/tilt_cap", "0.25"),
		op("add", "/steps/-", `"rollup2"`),
		op("remove", "/steps/8", ""),
		op("add", "/scorecards/debt/vetoes/debt.gilt", `["trig.x"]`),
		op("replace", "/scorecards/l1/signals/0/weight", "0.25"),
		op("replace", "/scorecards/l1/signals/1/weight", "0.25"),
		op("remove", "/review_calendar", ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.Scorecards["debt"].TiltCap != 0.25 || len(next.Steps) != 8 || next.Scorecards["debt"].Vetoes["debt.gilt"][0] != "trig.x" ||
		next.Scorecards["l1"].Signals[0].Weight != 0.25 || next.ReviewCalendar != nil {
		t.Fatalf("patched = %+v", next)
	}
	if c.Scorecards["debt"].TiltCap != 0.3 {
		t.Fatal("ApplyPatch changed its input")
	}

	ops, err := DiffConfigs(c, next)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ApplyPatch(c, ops)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalizeConfig(back), normalizeConfig(next)) {
		t.Fatalf("diff did not round-trip: %+v", ops)
	}
	if ops, _ := DiffConfigs(c, c); len(ops) != 0 {
		t.Fatalf("self diff = %+v", ops)
	}

	bad := []struct {
		ops    []PatchOp
		reason string
	}{
		{nil, "empty_patch"},
		{[]PatchOp{op("test", "/missing_score", "40")}, "test_failed"},
		{[]PatchOp{op("replace", "/nope", "1")}, "not_found"},
		{[]PatchOp{op("remove", "/steps/99", "")}, "not_found"},
		{[]PatchOp{op("move", "/steps/0", `"x"`)}, "unsupported_op"},
		{[]PatchOp{op("replace", "steps", `[]`)}, "invalid_pointer"},
		{[]PatchOp{op("replace", "/steps", "")}, "value_required"},
		{[]PatchOp{op("add", "/unknown_field", "1")}, "invalid_config"},
		{[]PatchOp{op("replace", "/missing_score", `"fifty"`)}, "invalid_config"},
	}
	for _, b := range bad {
		_, err := ApplyPatch(c, b.ops)
		e, ok := apperr.As(err)
		if !ok || e.Code != apperr.ValidationFailed || e.Details[0].Reason != b.reason {
			t.Errorf("%+v: want %s, got %v", b.ops, b.reason, err)
		}
	}
	// The failing operation's index is reported.
	_, err = ApplyPatch(c, []PatchOp{op("test", "/missing_score", "50"), op("replace", "/nope", "1")})
	if e, _ := apperr.As(err); e == nil || e.Details[0].Index == nil || *e.Details[0].Index != 1 {
		t.Fatalf("index = %v", err)
	}
}
