package derive

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"cloud.google.com/go/civil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/calendar"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
)

// SourceComputed is the source key cascade rows are written under.
const SourceComputed = "computed"

// earliest bounds a rebuild's history.
var earliest = civil.Date{Year: 1900, Month: time.January, Day: 1}

// CatalogReader is the part of the catalog the cascade reads.
type CatalogReader interface {
	Series(ctx context.Context, key string) (catalog.Series, error)
	ListSeries(ctx context.Context, f catalog.SeriesFilter) (paging.Page[catalog.Series], error)
	Downstream(ctx context.Context, ids []int64) ([]catalog.Series, error)
}

// BatchReader looks up what a write batch touched.
type BatchReader interface {
	Batch(ctx context.Context, id uuid.UUID) (datapoints.BatchSummary, error)
}

// CascadeDeps are the cascade's collaborators.
type CascadeDeps struct {
	DB         database.TxBeginner
	Catalog    CatalogReader
	Reader     datapoints.Reader
	Writer     datapoints.Writer
	Batches    BatchReader
	Registry   Registry
	Clock      clock.Clock
	Governance Governance
}

type cascade struct{ CascadeDeps }

// NewCascade returns the derive.cascade engine.
func NewCascade(d CascadeDeps) Cascade { return &cascade{d} }

// planItem is one series to recompute and its date range.
type planItem struct {
	series   catalog.Series
	from, to civil.Date
}

// Run computes the plan from the request and executes it series by series.
func (c *cascade) Run(ctx context.Context, req CascadeRequest, jobRunID int64, progress Progress) (CascadeResult, error) {
	switch req.Mode {
	case ModeIncremental:
		b, err := c.Batches.Batch(ctx, req.BatchID)
		if err != nil {
			return CascadeResult{}, err
		}
		return c.runIncremental(ctx, datapoints.WrittenEvent{
			BatchID: b.BatchID, SeriesIDs: b.SeriesIDs, MinDate: b.MinDate, MaxDate: b.MaxDate, Origin: b.Origin,
		}, jobRunID, progress)
	case ModeRebuild:
	default:
		return CascadeResult{}, fmt.Errorf("%w: unknown mode %q", ErrInvalidParams, req.Mode)
	}
	roots, err := c.resolveKeys(ctx, req.SeriesKeys)
	if err != nil {
		return CascadeResult{}, err
	}
	today := civil.DateOf(c.Clock.Now())
	items := make([]planItem, 0, len(roots))
	ids := make([]int64, 0, len(roots))
	for _, s := range roots {
		ids = append(ids, s.ID)
	}
	ordered := roots
	if req.IncludeDownstream {
		down, err := c.Catalog.Downstream(ctx, ids)
		if err != nil {
			return CascadeResult{}, err
		}
		ordered = mergeTopo(roots, down)
	}
	for _, s := range ordered {
		if s.Derivation != nil && s.Active {
			items = append(items, planItem{series: s, from: earliest, to: today})
		}
	}
	// A rebuild's rows become knowable now: older cutoffs keep the old rows.
	return c.execute(ctx, items, jobRunID, c.Clock.Now(), progress)
}

// runIncremental recomputes everything downstream of one write batch.
func (c *cascade) runIncremental(ctx context.Context, ev datapoints.WrittenEvent, jobRunID int64, progress Progress) (CascadeResult, error) {
	if ev.Origin == datapoints.OriginCascade || len(ev.SeriesIDs) == 0 {
		return CascadeResult{}, nil
	}
	down, err := c.Catalog.Downstream(ctx, ev.SeriesIDs)
	if err != nil {
		return CascadeResult{}, err
	}
	today := civil.DateOf(c.Clock.Now())
	return c.execute(ctx, planRanges(c.Registry, ev, down, today), jobRunID, time.Time{}, progress)
}

// planRanges computes each downstream series' affected range. Same-date
// derivations recompute [min, max]; windowed ones extend the end by their
// lookback (a revision at d moves every window containing d), capped at
// today. Ranges propagate along the topological order.
func planRanges(reg Registry, ev datapoints.WrittenEvent, down []catalog.Series, today civil.Date) []planItem {
	ranges := map[string]rng{}
	var items []planItem
	for _, s := range down {
		if s.Derivation == nil || !s.Active {
			continue
		}
		p, ok := reg.Get(s.Derivation.Fn)
		if !ok {
			continue
		}
		look := int(p.Lookback(s.Derivation.Params))
		var r *rng
		for _, key := range s.Derivation.Inputs {
			in, ok := ranges[key]
			if !ok {
				continue
			}
			r = widen(r, in.from, in.to)
		}
		// Inputs changed by the event itself (not by an upstream cascade
		// step) are not in ranges; their range is the event's.
		r = widen(r, ev.MinDate, ev.MaxDate)
		to := r.to
		if look > 0 {
			to = calendar.MonthEnd(calendar.AddMonths(to, look))
		}
		if to.After(today) {
			to = today
		}
		ranges[s.Key] = rng{r.from, to}
		items = append(items, planItem{series: s, from: r.from, to: to})
	}
	return items
}

type rng struct{ from, to civil.Date }

func widen(r *rng, from, to civil.Date) *rng {
	if r == nil {
		return &rng{from, to}
	}
	if from.Before(r.from) {
		r.from = from
	}
	if to.After(r.to) {
		r.to = to
	}
	return r
}

// execute computes and writes each series in order. knownAt zero means each
// row takes max(inputs.known_at).
func (c *cascade) execute(ctx context.Context, items []planItem, jobRunID int64, knownAt time.Time, progress Progress) (CascadeResult, error) {
	var res CascadeResult
	cutoff := c.Clock.Now() // one cutoff for the whole run
	for i, it := range items {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		pts, err := c.computeSeries(ctx, it, cutoff, knownAt)
		if err != nil {
			return res, fmt.Errorf("derive: %s: %w", it.series.Key, err)
		}
		w, err := c.write(ctx, pts, jobRunID)
		if err != nil {
			return res, fmt.Errorf("derive: write %s: %w", it.series.Key, err)
		}
		res.Series++
		res.RowsWritten += w.Written
		res.RowsSkipped += w.Skipped
		if progress != nil {
			progress((i+1)*100/len(items), it.series.Key)
		}
	}
	return res, nil
}

// computeSeries loads every input once for [from − lookback, to] and computes
// the series on each input observation date in [from, to].
func (c *cascade) computeSeries(ctx context.Context, it planItem, cutoff, knownAt time.Time) ([]datapoints.Point, error) {
	der := it.series.Derivation
	p, ok := c.Registry.Get(der.Fn)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownPrimitive, der.Fn)
	}
	look := int(p.Lookback(der.Params))
	loadFrom := it.from
	if look > 0 && it.from.After(earliest) {
		loadFrom = calendar.WindowStart(it.from, look+1)
	}
	opts := datapoints.ReadOpts{Cutoff: cutoff}
	full := Inputs{}
	dateSet := map[civil.Date]bool{}
	for alias, key := range der.Inputs {
		rows, err := c.Reader.Range(ctx, key, loadFrom, it.to, opts)
		if err != nil && !errors.Is(err, catalog.ErrNotFound) {
			return nil, err
		}
		full[alias] = rows
		for _, r := range rows {
			if !r.ObsDate.Before(it.from) && !r.ObsDate.After(it.to) {
				dateSet[r.ObsDate] = true
			}
		}
	}
	dates := make([]civil.Date, 0, len(dateSet))
	for d := range dateSet {
		dates = append(dates, d)
	}
	sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })

	env := Env{Series: it.series, Governance: c.Governance}
	var pts []datapoints.Point
	for _, d := range dates {
		in, ids, latest := sliceInputs(full, d, look)
		v, err := p.Compute(ctx, env, d, in, der.Params)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d, err)
		}
		if v.IsNull() {
			continue
		}
		ka := latest
		if !knownAt.IsZero() {
			ka = knownAt
		}
		pts = append(pts, datapoints.Point{
			SeriesKey: it.series.Key, ObsDate: d, Value: v, KnownAt: ka,
			Inputs: ids, DerivVer: der.Version,
		})
	}
	return pts, nil
}

// sliceInputs returns each input's history up to d (never later), the ids
// of the rows inside the lookback window (the row's lineage and input hash)
// and the latest known_at among them.
func sliceInputs(full Inputs, d civil.Date, look int) (Inputs, []int64, time.Time) {
	from := d
	if look > 0 {
		from = calendar.WindowStart(d, look+1)
	}
	in := make(Inputs, len(full))
	var ids []int64
	var latest time.Time
	for alias, rows := range full {
		n := sort.Search(len(rows), func(i int) bool { return rows[i].ObsDate.After(d) })
		in[alias] = rows[:n:n]
		for _, r := range rows[:n] {
			if r.ObsDate.Before(from) {
				continue
			}
			ids = append(ids, r.ID)
			if r.KnownAt.After(latest) {
				latest = r.KnownAt
			}
		}
	}
	slices.Sort(ids)
	return in, slices.Compact(ids), latest
}

// write stores one series' points in one transaction. A point whose
// known_at collides with an existing row (same inputs' known_at, different
// lineage, e.g. after a retraction) is retried as knowable now.
func (c *cascade) write(ctx context.Context, pts []datapoints.Point, jobRunID int64) (datapoints.WriteResult, error) {
	if len(pts) == 0 {
		return datapoints.WriteResult{}, nil
	}
	meta := datapoints.WriteMeta{Source: SourceComputed, Origin: datapoints.OriginCascade}
	if jobRunID != 0 { // 0: run outside the job system (tests, CLI)
		meta.JobRunID = &jobRunID
	}
	var res datapoints.WriteResult
	err := database.InTx(ctx, c.DB, func(tx pgx.Tx) error {
		r, err := c.Writer.Write(ctx, tx, meta, pts)
		if err != nil {
			return err
		}
		var retry []datapoints.Point
		for _, rj := range r.Rejected {
			if rj.Reason == datapoints.ReasonDuplicateKnownAt {
				p := pts[rj.Index]
				p.KnownAt = c.Clock.Now()
				retry = append(retry, p)
				continue
			}
			return fmt.Errorf("computed point rejected: %s %s: %s", rj.SeriesKey, rj.Reason, rj.Message)
		}
		if len(retry) > 0 {
			r2, err := c.Writer.Write(ctx, tx, meta, retry)
			if err != nil {
				return err
			}
			if len(r2.Rejected) > 0 {
				return fmt.Errorf("computed point rejected: %s %s", r2.Rejected[0].SeriesKey, r2.Rejected[0].Reason)
			}
			r.Written += r2.Written
		}
		res = r
		return nil
	})
	return res, err
}

func (c *cascade) resolveKeys(ctx context.Context, keys []string) ([]catalog.Series, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: rebuild needs series keys", ErrInvalidParams)
	}
	var out []catalog.Series
	seen := map[int64]bool{}
	req := paging.Request{Limit: paging.MaxLimit}
	for {
		page, err := c.Catalog.ListSeries(ctx, catalog.SeriesFilter{Keys: keys, Page: req})
		if err != nil {
			return nil, err
		}
		for _, s := range page.Items {
			if !seen[s.ID] {
				seen[s.ID] = true
				out = append(out, s)
			}
		}
		if page.Next == "" {
			break
		}
		req.After = page.Next
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no series match %v", catalog.ErrNotFound, keys)
	}
	return out, nil
}

// mergeTopo returns roots followed by downstream series, keeping downstream's
// topological order and dropping duplicates. Roots that depend on each other
// are re-sorted by the downstream order.
func mergeTopo(roots, down []catalog.Series) []catalog.Series {
	inDown := map[int64]bool{}
	for _, s := range down {
		inDown[s.ID] = true
	}
	out := make([]catalog.Series, 0, len(roots)+len(down))
	for _, s := range roots {
		if !inDown[s.ID] {
			out = append(out, s)
		}
	}
	return append(out, down...)
}
