package datapoints

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cloud.google.com/go/civil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/calendar"
)

// Lineage walks are bounded so a deep or wide graph cannot exhaust memory.
const (
	MaxLineageDepth = 10
	maxLineageNodes = 500
)

// sqlResolveRange is the point-in-time resolution: rows known by the cutoff,
// minus rows a retraction known by the cutoff hides; the latest per source;
// then the first source in priority order ($5).
const sqlResolveRange = `
WITH visible AS (
  SELECT d.id, d.obs_date, d.source_id, d.known_at, d.value_num, d.value_bool, d.value_cat,
         d.derivation_ver, d.inputs
  FROM dp.datapoints d
  WHERE d.series_id = $1 AND d.obs_date BETWEEN $2 AND $3
    AND d.status = 'active' AND d.known_at <= $4 AND d.source_id = ANY($5::bigint[])
    AND NOT EXISTS (SELECT 1 FROM dp.datapoints r
                    WHERE r.supersedes_id = d.id AND r.status = 'retracted' AND r.known_at <= $4)
), latest AS (
  SELECT DISTINCT ON (obs_date, source_id) *
  FROM visible
  ORDER BY obs_date, source_id, known_at DESC, id DESC
)
SELECT DISTINCT ON (obs_date) id, obs_date, source_id, known_at, value_num, value_bool, value_cat,
       derivation_ver, inputs
FROM latest
ORDER BY obs_date, array_position($5::bigint[], source_id)`

const datapointCols = `d.id, d.series_id, d.obs_date, d.known_at, d.value_num, d.value_bool, d.value_cat,
       d.source_id, d.status, d.derivation_ver, d.inputs, d.batch_id, d.job_run_id, d.actor_id,
       COALESCE(d.note, ''), d.supersedes_id, d.created_at`

var (
	sqlVersions = `SELECT ` + datapointCols + ` FROM dp.datapoints d
WHERE d.series_id = $1 AND d.obs_date = $2 ORDER BY d.known_at DESC, d.id DESC`
	sqlGetMany = `SELECT ` + datapointCols + ` FROM dp.datapoints d WHERE d.id = ANY($1::bigint[])`
)

const (
	sqlBatchSummary = `
SELECT array_agg(DISTINCT series_id ORDER BY series_id), min(obs_date), max(obs_date)
FROM dp.datapoints WHERE batch_id = $1`
	sqlBatchOrigin = `
SELECT payload->>'origin' FROM dp.outbox
WHERE topic = $1 AND payload->>'batch_id' = $2 ORDER BY id LIMIT 1`
)

func (s *Store) cutoff(o ReadOpts) time.Time {
	if o.Cutoff.IsZero() {
		return s.clock.Now()
	}
	return o.Cutoff
}

// readPlan is what one series read needs: its id and ordered source ids.
type readPlan struct {
	series   catalog.Series
	priority []int64
	keys     map[int64]string
}

func (s *Store) plan(ctx context.Context, key string, o ReadOpts) (readPlan, error) {
	ser, err := s.cat.Series(ctx, key)
	if err != nil {
		return readPlan{}, err
	}
	srcs, err := s.allSources(ctx)
	if err != nil {
		return readPlan{}, err
	}
	entries := ser.SourcePriority
	if len(o.Sources) > 0 {
		entries = o.Sources
	}
	keys := make(map[int64]string, len(srcs))
	for _, src := range srcs {
		keys[src.ID] = src.Key
	}
	return readPlan{series: ser, priority: priorityIDs(entries, srcs), keys: keys}, nil
}

func (s *Store) At(ctx context.Context, series string, d civil.Date, o ReadOpts) (*Resolved, error) {
	rs, err := s.Range(ctx, series, d, d, o)
	if err != nil || len(rs) == 0 {
		return nil, err
	}
	return &rs[0], nil
}

func (s *Store) Range(ctx context.Context, series string, from, to civil.Date, o ReadOpts) ([]Resolved, error) {
	if !from.IsValid() || !to.IsValid() || to.Before(from) {
		return nil, apperr.New(apperr.ValidationFailed, "invalid date range")
	}
	p, err := s.plan(ctx, series, o)
	if err != nil {
		return nil, err
	}
	if len(p.priority) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, sqlResolveRange, p.series.ID, dateTime(from), dateTime(to), s.cutoff(o).UTC(), p.priority)
	if err != nil {
		return nil, fmt.Errorf("datapoints: resolve: %w", err)
	}
	defer rows.Close()
	var out []Resolved
	for rows.Next() {
		var (
			r        Resolved
			obs      time.Time
			sourceID int64
			ver      *int32
		)
		if err := rows.Scan(&r.ID, &obs, &sourceID, &r.KnownAt, &r.Value.Num, &r.Value.Bool, &r.Value.Cat, &ver, &r.Inputs); err != nil {
			return nil, fmt.Errorf("datapoints: scan resolved: %w", err)
		}
		r.SeriesKey, r.ObsDate, r.Source = series, civil.DateOf(obs), p.keys[sourceID]
		if ver != nil {
			r.DerivVer = int(*ver)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// earliest is the lower bound for unbounded history reads.
var earliest = civil.Date{Year: 1900, Month: 1, Day: 1}

func (s *Store) History(ctx context.Context, series string, upTo civil.Date, window Months, o ReadOpts) ([]Resolved, error) {
	from := earliest
	if window > 0 {
		from = calendar.WindowStart(upTo, int(window))
	}
	return s.Range(ctx, series, from, upTo, o)
}

func (s *Store) Snapshot(ctx context.Context, series []string, d civil.Date, o ReadOpts) (map[string]Resolved, error) {
	out := make(map[string]Resolved, len(series))
	if o.Cutoff.IsZero() {
		o.Cutoff = s.clock.Now() // one cutoff for the whole snapshot
	}
	for _, key := range series {
		r, err := s.At(ctx, key, d, o)
		if errors.Is(err, catalog.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if r != nil {
			out[key] = *r
		}
	}
	return out, nil
}

func (s *Store) Versions(ctx context.Context, series string, d civil.Date) ([]Datapoint, error) {
	ser, err := s.cat.Series(ctx, series)
	if err != nil {
		return nil, err
	}
	return s.queryDatapoints(ctx, sqlVersions, ser.ID, dateTime(d))
}

func (s *Store) Get(ctx context.Context, id int64) (Datapoint, error) {
	dps, err := s.queryDatapoints(ctx, sqlGetMany, []int64{id})
	if err != nil {
		return Datapoint{}, err
	}
	if len(dps) == 0 {
		return Datapoint{}, apperr.Wrap(ErrNotFound, apperr.NotFound, fmt.Sprintf("datapoint %d not found", id))
	}
	return dps[0], nil
}

func (s *Store) Lineage(ctx context.Context, id int64, maxDepth int) (Lineage, error) {
	if maxDepth <= 0 || maxDepth > MaxLineageDepth {
		maxDepth = MaxLineageDepth
	}
	root, err := s.Get(ctx, id)
	if err != nil {
		return Lineage{}, err
	}
	// Load level by level, then assemble the tree from the fetched rows.
	rows := map[int64]Resolved{root.ID: root.Resolved}
	frontier := root.Inputs
	for depth := 1; depth <= maxDepth && len(frontier) > 0 && len(rows) < maxLineageNodes; depth++ {
		var want []int64
		for _, in := range frontier {
			if _, ok := rows[in]; !ok {
				want = append(want, in)
			}
		}
		if len(want) == 0 {
			break
		}
		if room := maxLineageNodes - len(rows); len(want) > room {
			want = want[:room]
		}
		dps, err := s.queryDatapoints(ctx, sqlGetMany, want)
		if err != nil {
			return Lineage{}, err
		}
		frontier = nil
		for _, dp := range dps {
			rows[dp.ID] = dp.Resolved
			frontier = append(frontier, dp.Inputs...)
		}
	}
	var build func(id int64, depth int) Lineage
	build = func(id int64, depth int) Lineage {
		l := Lineage{Datapoint: rows[id]}
		if depth == maxDepth {
			return l
		}
		for _, in := range rows[id].Inputs {
			if _, ok := rows[in]; ok {
				l.Inputs = append(l.Inputs, build(in, depth+1))
			}
		}
		return l
	}
	return build(root.ID, 0), nil
}

func (s *Store) Batch(ctx context.Context, batchID uuid.UUID) (BatchSummary, error) {
	var (
		ids      []int64
		min, max *time.Time
	)
	if err := s.pool.QueryRow(ctx, sqlBatchSummary, batchID).Scan(&ids, &min, &max); err != nil {
		return BatchSummary{}, fmt.Errorf("datapoints: batch: %w", err)
	}
	if len(ids) == 0 || min == nil || max == nil {
		return BatchSummary{}, apperr.Wrap(ErrNotFound, apperr.NotFound, "batch not found")
	}
	b := BatchSummary{BatchID: batchID, SeriesIDs: ids, MinDate: civil.DateOf(*min), MaxDate: civil.DateOf(*max), Origin: OriginPrimary}
	var origin *string
	err := s.pool.QueryRow(ctx, sqlBatchOrigin, TopicWritten, batchID.String()).Scan(&origin)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return BatchSummary{}, fmt.Errorf("datapoints: batch origin: %w", err)
	case origin != nil:
		b.Origin = Origin(*origin)
	}
	return b, nil
}

// queryDatapoints scans full rows and resolves series and source keys.
func (s *Store) queryDatapoints(ctx context.Context, sql string, args ...any) ([]Datapoint, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("datapoints: query: %w", err)
	}
	defer rows.Close()
	var (
		out      []Datapoint
		seriesOf []int64
		sourceOf []int64
	)
	for rows.Next() {
		var (
			dp       Datapoint
			seriesID int64
			sourceID int64
			obs      time.Time
			ver      *int32
		)
		if err := rows.Scan(&dp.ID, &seriesID, &obs, &dp.KnownAt, &dp.Value.Num, &dp.Value.Bool, &dp.Value.Cat,
			&sourceID, &dp.Status, &ver, &dp.Inputs, &dp.BatchID, &dp.JobRunID, &dp.ActorID, &dp.Note,
			&dp.SupersedesID, &dp.CreatedAt); err != nil {
			return nil, fmt.Errorf("datapoints: scan: %w", err)
		}
		dp.ObsDate = civil.DateOf(obs)
		if ver != nil {
			dp.DerivVer = int(*ver)
		}
		out = append(out, dp)
		seriesOf = append(seriesOf, seriesID)
		sourceOf = append(sourceOf, sourceID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("datapoints: query: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}
	series, err := s.cat.SeriesByIDs(ctx, seriesOf)
	if err != nil {
		return nil, err
	}
	srcKeys, err := s.sourceKeys(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].SeriesKey = series[seriesOf[i]].Key
		out[i].Source = srcKeys[sourceOf[i]]
	}
	return out, nil
}
