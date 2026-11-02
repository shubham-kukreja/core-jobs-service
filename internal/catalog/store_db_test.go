package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/testdb"
)

// fakeValidator accepts any fn except "bad" and reads exactly its inputs.
type fakeValidator struct{}

func (fakeValidator) ValidateDerivation(d catalog.Derivation) ([]string, error) {
	if d.Fn == "bad" {
		return nil, errors.New("unknown primitive")
	}
	keys := make([]string, 0, len(d.Inputs))
	for _, k := range d.Inputs {
		keys = append(keys, k)
	}
	return keys, nil
}

type fixture struct {
	t   *testing.T
	ctx context.Context
	db  *testdb.DB
	cat *catalog.Store
}

func setup(t *testing.T) *fixture {
	t.Helper()
	d := testdb.New(t)
	f := &fixture{t: t, ctx: context.Background(), db: d, cat: catalog.New(d.Runtime, fakeValidator{})}
	if _, err := d.Runtime.Exec(f.ctx, `INSERT INTO catalog.sources(key,kind,name) VALUES
		('override','override','Override'),('analyst','analyst','Analyst'),
		('file_import','file_import','File'),('computed','computed','Computed'),('nse','external_api','NSE')`); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) save(in catalog.SeriesInput) (catalog.SaveResult, error) {
	var res catalog.SaveResult
	err := database.InTx(f.ctx, f.db.Runtime, func(tx pgx.Tx) error {
		var err error
		res, err = f.cat.SaveSeries(f.ctx, tx, in)
		return err
	})
	return res, err
}

func (f *fixture) mustSave(in catalog.SeriesInput) catalog.SaveResult {
	f.t.Helper()
	res, err := f.save(in)
	if err != nil {
		f.t.Fatalf("save %s: %v", in.Key, err)
	}
	return res
}

func raw(key string) catalog.SeriesInput {
	return catalog.SeriesInput{Key: key, Name: key, Layer: catalog.LayerRaw, ValueType: catalog.ValueNum, Frequency: catalog.FreqMonthly}
}

func derived(key string, inputs map[string]string, params string) catalog.SeriesInput {
	return catalog.SeriesInput{Key: key, Name: key, Layer: catalog.LayerDerived, ValueType: catalog.ValueNum,
		Frequency: catalog.FreqMonthly, Derivation: &catalog.Derivation{Fn: "expr", Inputs: inputs, Params: json.RawMessage(params)}}
}

func TestEntities(t *testing.T) {
	f := setup(t)
	err := database.InTx(f.ctx, f.db.Runtime, func(tx pgx.Tx) error {
		if _, err := f.cat.SaveEntity(f.ctx, tx, catalog.Entity{Key: "l1.equity", Kind: "asset_class", Name: "Equity", Active: true}); err != nil {
			return err
		}
		_, err := f.cat.SaveEntity(f.ctx, tx, catalog.Entity{Key: "equity.large", Kind: "segment", Name: "Large Cap", ParentKey: "l1.equity", Active: true,
			Meta: map[string]any{"order": 1}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	e, err := f.cat.Entity(f.ctx, "equity.large")
	if err != nil || e.ParentKey != "l1.equity" || e.Meta["order"] != float64(1) {
		t.Fatalf("entity = %+v, %v", e, err)
	}
	segs, err := f.cat.ListEntities(f.ctx, "segment")
	if err != nil || len(segs) != 1 {
		t.Fatalf("segments = %v, %v", segs, err)
	}
	if _, err := f.cat.Entity(f.ctx, "nope"); !errors.Is(err, catalog.ErrNotFound) || !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("missing entity: %v", err)
	}
	err = database.InTx(f.ctx, f.db.Runtime, func(tx pgx.Tx) error {
		_, err := f.cat.SaveEntity(f.ctx, tx, catalog.Entity{Key: "x.y", Kind: "segment", Name: "X", ParentKey: "missing"})
		return err
	})
	if !apperr.Is(err, apperr.ValidationFailed) {
		t.Fatalf("unknown parent: %v", err)
	}
}

func TestSaveSeriesVersioningAndRebuild(t *testing.T) {
	f := setup(t)
	f.mustSave(raw("pe.midcap150"))
	f.mustSave(raw("pe.nifty100"))
	in := derived("spread.mid_large", map[string]string{"mid": "pe.midcap150", "large": "pe.nifty100"}, `{"expr":"mid - large"}`)

	first := f.mustSave(in)
	if !first.Rebuild || first.Series.Derivation.Version != 1 {
		t.Fatalf("new derivation: %+v", first)
	}
	again := f.mustSave(in)
	if again.Rebuild || again.Series.Derivation.Version != 1 {
		t.Fatalf("unchanged derivation must not rebuild: %+v", again)
	}
	in.Derivation.Params = json.RawMessage(`{"expr":"large - mid"}`)
	changed := f.mustSave(in)
	if !changed.Rebuild || changed.Series.Derivation.Version != 2 {
		t.Fatalf("changed derivation: %+v", changed)
	}

	edges, err := f.cat.Deps(f.ctx, "spread.mid_large", catalog.DepsUp, 0)
	if err != nil || len(edges) != 2 {
		t.Fatalf("deps up = %v, %v", edges, err)
	}
	down, err := f.cat.Deps(f.ctx, "pe.nifty100", catalog.DepsDown, 1)
	if err != nil || len(down) != 1 || down[0].SeriesKey != "spread.mid_large" {
		t.Fatalf("deps down = %v, %v", down, err)
	}
}

func TestSaveSeriesRejects(t *testing.T) {
	f := setup(t)
	f.mustSave(raw("a"))
	cases := map[string]struct {
		in   catalog.SeriesInput
		code apperr.Code
	}{
		"unknown input":  {derived("b", map[string]string{"x": "missing"}, `{}`), apperr.ValidationFailed},
		"bad primitive":  {catalog.SeriesInput{Key: "c", Name: "c", Layer: catalog.LayerDerived, ValueType: catalog.ValueNum, Frequency: catalog.FreqMonthly, Derivation: &catalog.Derivation{Fn: "bad", Inputs: map[string]string{"x": "a"}}}, apperr.ValidationFailed},
		"unknown entity": {func() catalog.SeriesInput { s := raw("d"); s.EntityKey = "nope"; return s }(), apperr.ValidationFailed},
		"unknown source": {func() catalog.SeriesInput { s := raw("e"); s.SourcePriority = []string{"bloomberg"}; return s }(), apperr.ValidationFailed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := f.save(tc.in); !apperr.Is(err, tc.code) {
				t.Fatalf("want %s, got %v", tc.code, err)
			}
		})
	}
}

func TestCycleRejectedAndRolledBack(t *testing.T) {
	f := setup(t)
	f.mustSave(raw("a"))
	f.mustSave(derived("b", map[string]string{"x": "a"}, `{"expr":"x"}`))
	f.mustSave(derived("c", map[string]string{"x": "b"}, `{"expr":"x"}`))

	// Turning a into a derivation of c closes a → b → c → a.
	in := derived("a", map[string]string{"x": "c"}, `{"expr":"x"}`)
	_, err := f.save(in)
	if !errors.Is(err, catalog.ErrCycle) || !apperr.Is(err, apperr.Conflict) {
		t.Fatalf("want cycle conflict, got %v", err)
	}
	a, err := f.cat.Series(f.ctx, "a")
	if err != nil || a.Layer != catalog.LayerRaw || a.Derivation != nil {
		t.Fatalf("rejected save must roll back: %+v %v", a, err)
	}
}

func TestDownstreamAndList(t *testing.T) {
	f := setup(t)
	f.mustSave(raw("pe.a"))
	f.mustSave(derived("x_1.b", map[string]string{"x": "pe.a"}, `{}`))
	f.mustSave(derived("x_1.c", map[string]string{"x": "x_1.b"}, `{}`))
	f.mustSave(derived("xx1.d", map[string]string{"x": "pe.a"}, `{}`))

	a, err := f.cat.Series(f.ctx, "pe.a")
	if err != nil {
		t.Fatal(err)
	}
	down, err := f.cat.Downstream(f.ctx, []int64{a.ID})
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, s := range down {
		pos[s.Key] = i
	}
	if len(down) != 3 || pos["x_1.b"] > pos["x_1.c"] {
		t.Fatalf("downstream order = %v", pos)
	}

	// '_' in the wildcard must not match 'x' in "xx1.d".
	page, err := f.cat.ListSeries(f.ctx, catalog.SeriesFilter{Keys: []string{"x_1.*"}})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("wildcard list = %+v, %v", page.Items, err)
	}
	p1, err := f.cat.ListSeries(f.ctx, catalog.SeriesFilter{Page: paging.Request{Limit: 2}})
	if err != nil || len(p1.Items) != 2 || p1.Next == "" {
		t.Fatalf("page 1 = %+v, %v", p1, err)
	}
	p2, err := f.cat.ListSeries(f.ctx, catalog.SeriesFilter{Page: paging.Request{Limit: 2, After: p1.Next}})
	if err != nil || len(p2.Items) != 2 || p2.Next != "" || p2.Items[0].Key <= p1.Items[1].Key {
		t.Fatalf("page 2 = %+v, %v", p2, err)
	}
	derivedOnly, err := f.cat.ListSeries(f.ctx, catalog.SeriesFilter{Layer: catalog.LayerDerived})
	if err != nil || len(derivedOnly.Items) != 3 {
		t.Fatalf("layer filter = %d, %v", len(derivedOnly.Items), err)
	}
}

func TestRegistryMetadataAndActive(t *testing.T) {
	f := setup(t)
	in := raw("pe.nifty50")
	in.Registry = catalog.RegistryMeta{DisplayName: "Nifty 50 P/E", NodeKey: "l1.equity", SignalKey: "valuation",
		Group: "equity", UpdateType: "raw", Owner: "System", Flags: []string{"mock"}}
	f.mustSave(in)
	page, err := f.cat.ListSeries(f.ctx, catalog.SeriesFilter{Group: "equity"})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("group filter = %+v, %v", page, err)
	}
	r := page.Items[0].Registry
	if r.DisplayName != "Nifty 50 P/E" || r.SignalKey != "valuation" || len(r.Flags) != 1 {
		t.Fatalf("registry = %+v", r)
	}
	err = database.InTx(f.ctx, f.db.Runtime, func(tx pgx.Tx) error {
		return f.cat.SetSeriesActive(f.ctx, tx, "missing", false)
	})
	if !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("set active on missing: %v", err)
	}
}
