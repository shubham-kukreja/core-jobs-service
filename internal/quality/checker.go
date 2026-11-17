package quality

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"cloud.google.com/go/civil"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/calendar"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
)

// SeriesLister lists catalog series.
type SeriesLister interface {
	ListSeries(ctx context.Context, f catalog.SeriesFilter) (paging.Page[catalog.Series], error)
}

// RangeReader reads resolved values.
type RangeReader interface {
	Range(ctx context.Context, series string, from, to civil.Date, o datapoints.ReadOpts) ([]datapoints.Resolved, error)
}

type checker struct {
	cat    SeriesLister
	rd     RangeReader
	minObs int // governance default for percentile min_obs
}

// NewChecker returns the quality checker. minObs is the governance default
// minimum observations for percentiles without their own min_obs.
func NewChecker(cat SeriesLister, rd RangeReader, minObs int) Checker {
	return &checker{cat: cat, rd: rd, minObs: minObs}
}

const (
	lookbackMonths = 24 // how far back to look for a series' latest value
	outlierHistory = 60 // observations the z-score compares against
	outlierMinObs  = 12
	badCatMonths   = 12
)

// ErrUnknownCheck is returned for a check name that does not exist.
var ErrUnknownCheck = errors.New("quality: unknown check")

var allChecks = []string{CheckStale, CheckMissingScores, CheckInsufficientHistory, CheckOutliers, CheckBadCategories}

// Check runs the requested checks over the requested (or all active) series
// as of r.AsOf. Lists are never nil so the JSON shape is stable.
func (c *checker) Check(ctx context.Context, r Request) (Report, error) {
	checks := r.Checks
	if len(checks) == 0 {
		checks = allChecks
	}
	for _, ch := range checks {
		if !slices.Contains(allChecks, ch) {
			return Report{}, fmt.Errorf("%w %q", ErrUnknownCheck, ch)
		}
	}
	on := func(ch string) bool { return slices.Contains(checks, ch) }
	series, err := c.series(ctx, r.Series)
	if err != nil {
		return Report{}, err
	}
	rep := Report{AsOf: r.AsOf, Stale: []Stale{}, Outliers: []Outlier{}, InsufficientHistory: []InsufficientHistory{},
		MissingScores: []string{}, BadCategories: []BadCategory{}}
	opts := datapoints.ReadOpts{} // latest knowledge
	for _, s := range series {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		primary := s.Layer == catalog.LayerRaw || s.Layer == catalog.LayerJudgment
		needRows := (primary && (on(CheckStale) || on(CheckOutliers) || on(CheckBadCategories))) ||
			(s.Layer == catalog.LayerScore && on(CheckMissingScores))
		var rows []datapoints.Resolved
		if needRows {
			if rows, err = c.rd.Range(ctx, s.Key, calendar.AddMonths(r.AsOf, -max(lookbackMonths, outlierHistory)), r.AsOf, opts); err != nil {
				return rep, err
			}
		}
		expected, hasPeriod := expectedPeriod(s.Frequency, r.AsOf)
		switch {
		case primary:
			if on(CheckStale) && hasPeriod {
				if st, ok := staleness(s, rows, expected); ok {
					rep.Stale = append(rep.Stale, st)
				}
			}
			if on(CheckOutliers) && s.ValueType == catalog.ValueNum {
				if o, ok := outlier(s.Key, rows); ok {
					rep.Outliers = append(rep.Outliers, o)
				}
			}
			if on(CheckBadCategories) && s.ValueType == catalog.ValueCat && len(s.AllowedValues) > 0 {
				rep.BadCategories = append(rep.BadCategories, badCategories(s, rows, calendar.AddMonths(r.AsOf, -badCatMonths))...)
			}
		case s.Layer == catalog.LayerScore && on(CheckMissingScores) && hasPeriod:
			if len(rows) == 0 || rows[len(rows)-1].ObsDate.Before(expected) {
				rep.MissingScores = append(rep.MissingScores, s.Key)
			}
		case s.Layer == catalog.LayerPercentile && on(CheckInsufficientHistory):
			ih, ok, err := c.insufficient(ctx, s, r.AsOf, opts)
			if err != nil {
				return rep, err
			}
			if ok {
				rep.InsufficientHistory = append(rep.InsufficientHistory, ih)
			}
		}
	}
	return rep, nil
}

func (c *checker) series(ctx context.Context, keys []string) ([]catalog.Series, error) {
	var out []catalog.Series
	req := paging.Request{Limit: paging.MaxLimit}
	for {
		page, err := c.cat.ListSeries(ctx, catalog.SeriesFilter{Keys: keys, Page: req})
		if err != nil {
			return nil, err
		}
		for _, s := range page.Items {
			if s.Active {
				out = append(out, s)
			}
		}
		if page.Next == "" {
			return out, nil
		}
		req.After = page.Next
	}
}

// expectedPeriod returns the latest period end on or before asOf that a
// series of this frequency should have a value for.
func expectedPeriod(f catalog.Frequency, asOf civil.Date) (civil.Date, bool) {
	switch f {
	case catalog.FreqDaily:
		d := asOf
		for wd := d.In(time.UTC).Weekday(); wd == time.Saturday || wd == time.Sunday; wd = d.In(time.UTC).Weekday() {
			d = d.AddDays(-1)
		}
		return d, true
	case catalog.FreqWeekly:
		return asOf.AddDays(-6), true // any value in the last 7 days
	case catalog.FreqMonthly:
		if calendar.IsMonthEnd(asOf) {
			return asOf, true
		}
		return calendar.MonthEnd(calendar.AddMonths(asOf, -1)), true
	case catalog.FreqQuarterly:
		m := calendar.MonthEnd(asOf)
		if !calendar.IsMonthEnd(asOf) {
			m = calendar.MonthEnd(calendar.AddMonths(asOf, -1))
		}
		for m.Month%3 != 0 {
			m = calendar.MonthEnd(calendar.AddMonths(m, -1))
		}
		return m, true
	}
	return civil.Date{}, false // event series have no expected period
}

// staleness flags a series whose latest value is older than the start of
// the expected period.
func staleness(s catalog.Series, rows []datapoints.Resolved, expected civil.Date) (Stale, bool) {
	var last *civil.Date
	if len(rows) > 0 {
		d := rows[len(rows)-1].ObsDate
		last = &d
	}
	start := expected
	switch s.Frequency {
	case catalog.FreqMonthly:
		start = civil.Date{Year: expected.Year, Month: expected.Month, Day: 1}
	case catalog.FreqQuarterly:
		start = calendar.AddMonths(civil.Date{Year: expected.Year, Month: expected.Month, Day: 1}, -2)
	}
	if last != nil && !last.Before(start) {
		return Stale{}, false
	}
	return Stale{Series: s.Key, LastObs: last, ExpectedBy: expected}, true
}

// outlier z-scores the latest value against up to 60 earlier observations.
func outlier(key string, rows []datapoints.Resolved) (Outlier, bool) {
	var vals []float64
	for _, r := range rows {
		if r.Value.Num != nil {
			vals = append(vals, *r.Value.Num)
		}
	}
	if len(vals) < outlierMinObs+1 {
		return Outlier{}, false
	}
	latest := vals[len(vals)-1]
	hist := vals[max(0, len(vals)-1-outlierHistory) : len(vals)-1]
	var mean float64
	for _, v := range hist {
		mean += v
	}
	mean /= float64(len(hist))
	var ss float64
	for _, v := range hist {
		ss += (v - mean) * (v - mean)
	}
	sd := math.Sqrt(ss / float64(len(hist)-1))
	if sd == 0 {
		return Outlier{}, false
	}
	z := (latest - mean) / sd
	if math.Abs(z) <= OutlierZ {
		return Outlier{}, false
	}
	var d civil.Date
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Value.Num != nil {
			d = rows[i].ObsDate
			break
		}
	}
	return Outlier{Series: key, ObsDate: d, Value: latest, Z: math.Round(z*100) / 100}, true
}

func badCategories(s catalog.Series, rows []datapoints.Resolved, from civil.Date) []BadCategory {
	var out []BadCategory
	for _, r := range rows {
		if r.Value.Cat == nil || r.ObsDate.Before(from) || slices.Contains(s.AllowedValues, *r.Value.Cat) {
			continue
		}
		out = append(out, BadCategory{Series: s.Key, ObsDate: r.ObsDate, Value: *r.Value.Cat})
	}
	return out
}

// insufficient counts a percentile series' input observations in its window.
func (c *checker) insufficient(ctx context.Context, s catalog.Series, asOf civil.Date, o datapoints.ReadOpts) (InsufficientHistory, bool, error) {
	if s.Derivation == nil || s.Derivation.Fn != "percentile_rank" {
		return InsufficientHistory{}, false, nil
	}
	var p struct {
		WindowMonths int `json:"window_months"`
		MinObs       int `json:"min_obs"`
	}
	if err := json.Unmarshal(s.Derivation.Params, &p); err != nil || p.WindowMonths < 1 {
		return InsufficientHistory{}, false, nil
	}
	minObs := p.MinObs
	if minObs == 0 {
		minObs = c.minObs
	}
	in, ok := s.Derivation.Inputs["x"]
	if !ok {
		return InsufficientHistory{}, false, nil
	}
	rows, err := c.rd.Range(ctx, in, calendar.WindowStart(asOf, p.WindowMonths), asOf, o)
	if err != nil {
		return InsufficientHistory{}, false, err
	}
	n := 0
	for _, r := range rows {
		if r.Value.Num != nil {
			n++
		}
	}
	if n >= minObs {
		return InsufficientHistory{}, false, nil
	}
	return InsufficientHistory{Series: s.Key, Obs: n, Min: minObs}, true, nil
}

// ---- job -------------------------------------------------------------------

// KindCheck is Job 5.
const KindCheck = "quality.check"

// CheckArgs are quality.check's arguments; as_of defaults to today.
type CheckArgs struct {
	AsOf   *civil.Date `json:"as_of,omitempty"`
	Series []string    `json:"series,omitempty"`
	Checks []string    `json:"checks,omitempty"`
}

func (CheckArgs) Kind() string { return KindCheck }

// CheckSpec is quality.check's static definition.
var CheckSpec = jobs.Spec{
	Kind:            KindCheck,
	Queue:           jobs.QueueCompute,
	Triggers:        []jobs.TriggerType{jobs.TriggerSchedule, jobs.TriggerUser},
	UserTriggerable: true,
	Roles:           []string{"analyst", "admin"},
	MaxAttempts:     3,
	Timeout:         5 * time.Minute,
	Concurrency:     1,
	ArgsVersion:     1,
	Unique:          &jobs.UniqueOpts{Period: 10 * time.Minute},
}

// RegisterCheck registers quality.check. The report is the job result, so
// the latest report is the latest succeeded run.
func RegisterCheck(r *jobs.Registry, c Checker, clk clock.Clock) {
	jobs.Register(r, CheckSpec, jobs.HandlerFunc[CheckArgs](func(ctx context.Context, _ *jobs.RunCtx, a CheckArgs) (jobs.Outcome, error) {
		asOf := civil.DateOf(clk.Now())
		if a.AsOf != nil {
			asOf = *a.AsOf
		}
		rep, err := c.Check(ctx, Request{AsOf: asOf, Series: a.Series, Checks: a.Checks})
		if errors.Is(err, ErrUnknownCheck) {
			return jobs.Outcome{}, jobs.Permanent(err)
		}
		if err != nil {
			return jobs.Outcome{}, err
		}
		b, err := json.Marshal(rep)
		if err != nil {
			return jobs.Outcome{}, err
		}
		var sum map[string]any
		if err := json.Unmarshal(b, &sum); err != nil {
			return jobs.Outcome{}, err
		}
		return jobs.Outcome{Summary: sum}, nil
	}))
}
