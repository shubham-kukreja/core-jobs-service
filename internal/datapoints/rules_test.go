package datapoints

import (
	"bytes"
	"math"
	"reflect"
	"testing"
	"time"

	"cloud.google.com/go/civil"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
)

func TestPriorityIDs(t *testing.T) {
	srcs := []catalog.Source{
		{ID: 1, Key: "override", Kind: catalog.SourceOverride},
		{ID: 2, Key: "analyst", Kind: catalog.SourceAnalyst},
		{ID: 3, Key: "rbi", Kind: catalog.SourceExternalAPI},
		{ID: 4, Key: "fred", Kind: catalog.SourceExternalAPI},
		{ID: 5, Key: "computed", Kind: catalog.SourceComputed},
	}
	got := priorityIDs([]string{"override", "analyst", "external_api", "file_import", "computed"}, srcs)
	if want := []int64{1, 2, 4, 3, 5}; !reflect.DeepEqual(got, want) { // kinds expand by key: fred < rbi
		t.Fatalf("got %v, want %v", got, want)
	}
	// A specific key ahead of its kind keeps its position; no duplicates.
	got = priorityIDs([]string{"rbi", "external_api", "unknown"}, srcs)
	if want := []int64{3, 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestWritable(t *testing.T) {
	cases := []struct {
		k    catalog.SourceKind
		l    catalog.Layer
		want bool
	}{
		{catalog.SourceAnalyst, catalog.LayerJudgment, true},
		{catalog.SourceAnalyst, catalog.LayerScore, false},
		{catalog.SourceAnalyst, catalog.LayerTrigger, true},
		{catalog.SourceFileImport, catalog.LayerRaw, true},
		{catalog.SourceFileImport, catalog.LayerPercentile, false},
		{catalog.SourceExternalAPI, catalog.LayerJudgment, false},
		{catalog.SourceComputed, catalog.LayerRaw, false},
		{catalog.SourceComputed, catalog.LayerVeto, true},
		{catalog.SourceOverride, catalog.LayerScore, true},
		{catalog.SourceOverride, catalog.LayerRaw, true},
	}
	for _, c := range cases {
		if got := writable(c.k, c.l); got != c.want {
			t.Errorf("writable(%s, %s) = %v", c.k, c.l, got)
		}
	}
}

func TestCheckPoint(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	lo, hi := 0.0, 100.0
	num := catalog.Series{Key: "pe", Layer: catalog.LayerRaw, ValueType: catalog.ValueNum, Active: true,
		Validation: catalog.Validation{Min: &lo, Max: &hi}}
	cat := catalog.Series{Key: "stance", Layer: catalog.LayerJudgment, ValueType: catalog.ValueCat, Active: true,
		AllowedValues: []string{"cutting", "hold", "hiking"}}
	pct := catalog.Series{Key: "pct", Layer: catalog.LayerPercentile, ValueType: catalog.ValueNum, Active: true}
	analyst := catalog.Source{Key: "analyst", Kind: catalog.SourceAnalyst}
	computed := catalog.Source{Key: "computed", Kind: catalog.SourceComputed}
	d := civil.Date{Year: 2026, Month: 10, Day: 31}

	cases := map[string]struct {
		s    catalog.Series
		src  catalog.Source
		p    Point
		want string
	}{
		"ok num":         {num, analyst, Point{ObsDate: d, Value: Num(22.5)}, ""},
		"ok cat":         {cat, analyst, Point{ObsDate: d, Value: Cat("hold")}, ""},
		"computed null":  {pct, computed, Point{ObsDate: d}, ""},
		"analyst null":   {num, analyst, Point{ObsDate: d}, ReasonWrongType},
		"wrong type":     {num, analyst, Point{ObsDate: d, Value: Bool(true)}, ReasonWrongType},
		"two values":     {num, analyst, Point{ObsDate: d, Value: Value{Num: Num(1).Num, Bool: Bool(true).Bool}}, ReasonWrongType},
		"nan":            {num, analyst, Point{ObsDate: d, Value: Num(math.NaN())}, ReasonWrongType},
		"inf":            {num, analyst, Point{ObsDate: d, Value: Num(math.Inf(1))}, ReasonWrongType},
		"above max":      {num, analyst, Point{ObsDate: d, Value: Num(101)}, ReasonOutOfRange},
		"below min":      {num, analyst, Point{ObsDate: d, Value: Num(-1)}, ReasonOutOfRange},
		"bad category":   {cat, analyst, Point{ObsDate: d, Value: Cat("pausing")}, ReasonNotAllowed},
		"analyst pctile": {pct, analyst, Point{ObsDate: d, Value: Num(50)}, ReasonNotWritable},
		"inactive":       {catalog.Series{Layer: catalog.LayerRaw, ValueType: catalog.ValueNum}, analyst, Point{ObsDate: d, Value: Num(1)}, ReasonInactiveSeries},
		"bad date":       {num, analyst, Point{ObsDate: civil.Date{Year: 2026, Month: 2, Day: 30}, Value: Num(1)}, ReasonInvalidDate},
		"future known":   {num, analyst, Point{ObsDate: d, Value: Num(1), KnownAt: now.Add(time.Hour)}, ReasonInvalidKnownAt},
		"skew ok":        {num, analyst, Point{ObsDate: d, Value: Num(1), KnownAt: now.Add(30 * time.Second)}, ""},
		"long note":      {num, analyst, Point{ObsDate: d, Value: Num(1), Note: string(make([]byte, 2001))}, ReasonNoteTooLong},
	}
	for name, c := range cases {
		if got, _ := checkPoint(c.s, c.src, c.p, now); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}

func TestInputHash(t *testing.T) {
	a := inputHash(1, []int64{3, 1, 2})
	if !bytes.Equal(a, inputHash(1, []int64{1, 2, 3})) {
		t.Fatal("hash must not depend on input order")
	}
	if bytes.Equal(a, inputHash(2, []int64{1, 2, 3})) {
		t.Fatal("hash must change with derivation version")
	}
	if bytes.Equal(a, inputHash(1, []int64{1, 2, 4})) {
		t.Fatal("hash must change with inputs")
	}
	if len(a) != 32 {
		t.Fatal("sha256 expected")
	}
}

func TestSameValue(t *testing.T) {
	if !sameValue(Num(1), Num(1)) || sameValue(Num(1), Num(2)) || sameValue(Num(1), Value{}) ||
		!sameValue(Value{}, Value{}) || !sameValue(Cat("a"), Cat("a")) || sameValue(Bool(true), Bool(false)) {
		t.Fatal("sameValue misbehaves")
	}
}
