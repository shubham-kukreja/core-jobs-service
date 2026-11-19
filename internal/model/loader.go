package model

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"cloud.google.com/go/civil"
	"github.com/google/uuid"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
)

// Lookbacks for an input's latest value at or before as_of. A score older
// than this counts as missing (it scores missing_score, with a warning); an
// analyst trigger such as an AMC restriction stays in force for a year
// unless a newer value clears it.
const (
	ScoreLookback   datapoints.Months = 3
	TriggerLookback datapoints.Months = 12
	historyRuns                       = 20
)

// HistoryStore reads what the satellite rules need from earlier runs.
type HistoryStore interface {
	PublishedHistory(ctx context.Context, model string, asOf civil.Date, n int) ([]Run, error)
	RunValues(ctx context.Context, id uuid.UUID, step string) ([]Node, error)
	ActiveOverrides(ctx context.Context, model string, asOf civil.Date) ([]NodeOverride, error)
}

type loader struct {
	reader  datapoints.Reader
	history HistoryStore
	clock   clock.Clock
}

// NewLoader returns the InputLoader.
func NewLoader(r datapoints.Reader, h HistoryStore, clk clock.Clock) InputLoader {
	return loader{reader: r, history: h, clock: clk}
}

func (l loader) Load(ctx context.Context, r LoadRequest) (EvalInput, error) {
	if r.Cutoff.IsZero() {
		r.Cutoff = l.clock.Now()
	}
	in := EvalInput{AsOf: r.AsOf, Cutoff: r.Cutoff, Config: r.Config, Profile: r.Profile, Params: r.Params, Inputs: map[string]Input{}}
	o := datapoints.ReadOpts{Cutoff: r.Cutoff}
	for key, path := range ReferencedSeries(r.Config) {
		look := ScoreLookback
		if strings.Contains(path, "/vetoes/") {
			look = TriggerLookback
		}
		rows, err := l.reader.History(ctx, key, r.AsOf, look, o)
		if errors.Is(err, catalog.ErrNotFound) {
			continue // the engine warns about a missing score
		}
		if err != nil {
			return in, err
		}
		if len(rows) == 0 {
			continue
		}
		last := rows[len(rows)-1]
		obs := last.ObsDate
		in.Inputs[key] = Input{Value: last.Value, DatapointID: last.ID, Source: last.Source, ObservedAt: &obs}
	}
	ovs, err := l.history.ActiveOverrides(ctx, r.Model, r.AsOf)
	if err != nil {
		return in, err
	}
	in.Overrides = ovs
	if in.History, err = l.satelliteHistory(ctx, r); err != nil {
		return in, err
	}
	return in, nil
}

// satelliteHistory counts, over the reviews before as_of (newest first;
// one published run per date), how many consecutive ones held each sector,
// and how many consecutive ones had each bucket's composite outside
// extreme_full_cap's band.
func (l loader) satelliteHistory(ctx context.Context, r LoadRequest) (SatelliteHistory, error) {
	h := SatelliteHistory{QuartersHeld: map[string]int{}, ExtremeRuns: map[string]int{}}
	all, err := l.history.PublishedHistory(ctx, r.Model, r.AsOf, historyRuns)
	if err != nil || len(all) == 0 {
		return h, err
	}
	// One review per as_of date: the newest published run for each.
	var runs []Run
	for _, run := range all {
		if len(runs) == 0 || runs[len(runs)-1].AsOf != run.AsOf {
			runs = append(runs, run)
		}
	}
	if sat := r.Config.SectorSatellite; sat != nil {
		for _, sec := range sat.Sectors {
			for _, run := range runs {
				if !slices.Contains(run.Params.SectorsHeld, sec) {
					break
				}
				h.QuartersHeld[sec]++
			}
		}
	}
	var band *extremeParams
	for _, c := range r.Config.Constraints {
		if c.Name == "extreme_full_cap" {
			var x extremeParams
			if err := json.Unmarshal(c.Params, &x); err == nil {
				band = &x
			}
		}
	}
	if band == nil {
		return h, nil
	}
	open := map[string]bool{} // buckets whose streak is unbroken so far
	for _, name := range scorecardOrder(r.Config) {
		for _, b := range r.Config.Scorecards[name].Buckets {
			open[b] = true
		}
	}
	for _, run := range runs {
		nodes, err := l.history.RunValues(ctx, run.UUID, "composite")
		if err != nil {
			return h, err
		}
		comp := map[string]float64{}
		for _, n := range nodes {
			if n.Num != nil {
				comp[strings.TrimSuffix(n.Key, "::composite")] = *n.Num
			}
		}
		for b := range open {
			c, ok := comp[b]
			if !ok || (c <= band.High && c >= band.Low) {
				delete(open, b)
				continue
			}
			h.ExtremeRuns[b]++
		}
		if len(open) == 0 {
			break
		}
	}
	return h, nil
}
