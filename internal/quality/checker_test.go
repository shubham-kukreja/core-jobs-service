package quality

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"cloud.google.com/go/civil"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/calendar"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
)

type fakeCat []catalog.Series

func (f fakeCat) ListSeries(_ context.Context, _ catalog.SeriesFilter) (paging.Page[catalog.Series], error) {
	return paging.Page[catalog.Series]{Items: f}, nil
}

type fakeRd map[string][]datapoints.Resolved

func (f fakeRd) Range(_ context.Context, key string, from, to civil.Date, _ datapoints.ReadOpts) ([]datapoints.Resolved, error) {
	var out []datapoints.Resolved
	for _, r := range f[key] {
		if !r.ObsDate.Before(from) && !r.ObsDate.After(to) {
			out = append(out, r)
		}
	}
	return out, nil
}

func me(y, m int) civil.Date {
	return calendar.MonthEnd(civil.Date{Year: y, Month: time.Month(m), Day: 1})
}

// months returns n month-end numeric values ending at end.
func months(end civil.Date, vals ...float64) []datapoints.Resolved {
	out := make([]datapoints.Resolved, len(vals))
	for i, v := range vals {
		out[i] = datapoints.Resolved{ObsDate: calendar.MonthEnd(calendar.AddMonths(end, i-len(vals)+1)), Value: datapoints.Num(v)}
	}
	return out
}

func TestCheckReport(t *testing.T) {
	asOf := civil.Date{Year: 2026, Month: 11, Day: 14}
	oct, sep := me(2026, 10), me(2026, 9)
	steady := make([]float64, 20)
	for i := range steady {
		steady[i] = 100 + float64(i%3)
	}
	spike := append(append([]float64{}, steady...), 500)
	cat := datapoints.Cat
	cats := fakeCat{
		{Key: "flows.small", Layer: catalog.LayerRaw, ValueType: catalog.ValueNum, Frequency: catalog.FreqMonthly, Active: true},
		{Key: "pe.nifty100", Layer: catalog.LayerRaw, ValueType: catalog.ValueNum, Frequency: catalog.FreqMonthly, Active: true},
		{Key: "judg.rbi.stance", Layer: catalog.LayerJudgment, ValueType: catalog.ValueCat, Frequency: catalog.FreqMonthly,
			AllowedValues: []string{"cutting", "hold", "hiking"}, Active: true},
		{Key: "score.eq.large.val", Layer: catalog.LayerScore, ValueType: catalog.ValueNum, Frequency: catalog.FreqMonthly, Active: true},
		{Key: "score.eq.mid.val", Layer: catalog.LayerScore, ValueType: catalog.ValueNum, Frequency: catalog.FreqMonthly, Active: true},
		{Key: "pct.pe", Layer: catalog.LayerPercentile, ValueType: catalog.ValueNum, Frequency: catalog.FreqMonthly, Active: true,
			Derivation: &catalog.Derivation{Fn: "percentile_rank", Inputs: map[string]string{"x": "pe.nifty100"},
				Params: json.RawMessage(`{"window_months":180,"min_obs":24}`)}},
		{Key: "retired", Layer: catalog.LayerRaw, Frequency: catalog.FreqMonthly, Active: false},
	}
	rd := fakeRd{
		"flows.small":        months(sep, 1, 2, 3), // no October value → stale
		"pe.nifty100":        months(oct, spike...),
		"judg.rbi.stance":    {{ObsDate: sep, Value: cat("hold")}, {ObsDate: oct, Value: cat("pausing")}},
		"score.eq.large.val": months(oct, 60),
		"score.eq.mid.val":   months(sep, 55),
	}
	rep, err := NewChecker(cats, rd, 24).Check(context.Background(), Request{AsOf: asOf})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Stale) != 1 || rep.Stale[0].Series != "flows.small" || rep.Stale[0].ExpectedBy != oct || rep.Stale[0].LastObs != sep {
		t.Errorf("stale: %+v", rep.Stale)
	}
	if len(rep.Outliers) != 1 || rep.Outliers[0].Series != "pe.nifty100" || rep.Outliers[0].Z <= OutlierZ {
		t.Errorf("outliers: %+v", rep.Outliers)
	}
	if len(rep.BadCategories) != 1 || rep.BadCategories[0].Value != "pausing" {
		t.Errorf("bad categories: %+v", rep.BadCategories)
	}
	if len(rep.MissingScores) != 1 || rep.MissingScores[0] != "score.eq.mid.val" {
		t.Errorf("missing scores: %+v", rep.MissingScores)
	}
	if len(rep.InsufficientHistory) != 1 || rep.InsufficientHistory[0].Obs != 21 || rep.InsufficientHistory[0].Min != 24 {
		t.Errorf("insufficient: %+v", rep.InsufficientHistory)
	}

	only, err := NewChecker(cats, rd, 24).Check(context.Background(), Request{AsOf: asOf, Checks: []string{CheckStale}})
	if err != nil || len(only.Stale) != 1 || len(only.Outliers)+len(only.MissingScores) != 0 || only.Outliers == nil {
		t.Errorf("stale only: %+v %v", only, err)
	}
	if _, err := NewChecker(cats, rd, 24).Check(context.Background(), Request{AsOf: asOf, Checks: []string{"nope"}}); !errors.Is(err, ErrUnknownCheck) {
		t.Errorf("unknown check: %v", err)
	}
}

func TestExpectedPeriod(t *testing.T) {
	sat := civil.Date{Year: 2026, Month: 11, Day: 14}
	cases := []struct {
		f    catalog.Frequency
		asOf civil.Date
		want civil.Date
	}{
		{catalog.FreqDaily, sat, civil.Date{Year: 2026, Month: 11, Day: 13}},
		{catalog.FreqMonthly, me(2026, 10), me(2026, 10)},
		{catalog.FreqMonthly, sat, me(2026, 10)},
		{catalog.FreqQuarterly, sat, me(2026, 9)},
		{catalog.FreqWeekly, sat, civil.Date{Year: 2026, Month: 11, Day: 8}},
	}
	for _, c := range cases {
		if got, ok := expectedPeriod(c.f, c.asOf); !ok || got != c.want {
			t.Errorf("%s %s: got %s", c.f, c.asOf, got)
		}
	}
	if _, ok := expectedPeriod("event", sat); ok {
		t.Error("event series have no expected period")
	}
}

func TestCheckJob(t *testing.T) {
	r := jobs.NewRegistry()
	cats := fakeCat{{Key: "flows.small", Layer: catalog.LayerRaw, ValueType: catalog.ValueNum, Frequency: catalog.FreqMonthly, Active: true}}
	RegisterCheck(r, NewChecker(cats, fakeRd{}, 24), clock.NewFake(time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC)))
	k, ok := r.Get(KindCheck)
	if !ok {
		t.Fatal("not registered")
	}
	out, err := k.Invoke(context.Background(), &jobs.RunCtx{}, json.RawMessage(`{"checks":["stale"]}`))
	if err != nil || out.Summary["as_of"] != "2026-11-14" || len(out.Summary["stale"].([]any)) != 1 {
		t.Fatalf("got %+v, %v", out.Summary, err)
	}
	if _, err := k.Invoke(context.Background(), &jobs.RunCtx{}, json.RawMessage(`{"checks":["nope"]}`)); !jobs.IsPermanent(err) {
		t.Fatalf("unknown check must be permanent: %v", err)
	}
}
