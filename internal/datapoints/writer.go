package datapoints

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/civil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
)

const sqlCurrentBySource = `
SELECT DISTINCT ON (d.series_id, d.obs_date)
       d.series_id, d.obs_date, d.value_num, d.value_bool, d.value_cat, d.input_hash
FROM dp.datapoints d
JOIN unnest($1::bigint[], $2::date[]) AS k(series_id, obs_date)
  ON d.series_id = k.series_id AND d.obs_date = k.obs_date
WHERE d.source_id = $3 AND d.status = 'active'
  AND NOT EXISTS (SELECT 1 FROM dp.datapoints r WHERE r.supersedes_id = d.id AND r.status = 'retracted')
ORDER BY d.series_id, d.obs_date, d.known_at DESC, d.id DESC`

const sqlInsertPoints = `
INSERT INTO dp.datapoints (series_id, obs_date, known_at, value_num, value_bool, value_cat, source_id,
                           derivation_ver, input_hash, inputs, batch_id, job_run_id, actor_id, note)
SELECT u.series_id, u.obs_date, u.known_at, u.value_num, u.value_bool, u.value_cat, $9,
       u.derivation_ver, u.input_hash, NULLIF(u.inputs, '')::bigint[], $10, $11, $12, NULLIF(u.note, '')
FROM unnest($1::bigint[], $2::date[], $3::timestamptz[], $4::float8[], $5::bool[], $6::text[],
            $7::int4[], $8::bytea[], $13::text[], $14::text[])
     AS u(series_id, obs_date, known_at, value_num, value_bool, value_cat, derivation_ver, input_hash, inputs, note)
ON CONFLICT (series_id, obs_date, source_id, known_at) WHERE status = 'active' DO NOTHING
RETURNING id, series_id, obs_date`

const sqlInsertOutbox = `INSERT INTO dp.outbox (topic, payload) VALUES ($1, $2)`

type pointKey struct {
	seriesID int64
	date     civil.Date
}

// pending is a validated point awaiting insert.
type pending struct {
	index  int
	series catalog.Series
	p      Point
	hash   []byte
}

func (s *Store) Write(ctx context.Context, tx pgx.Tx, meta WriteMeta, pts []Point) (WriteResult, error) {
	if len(pts) > MaxBatch {
		return WriteResult{}, apperr.New(apperr.ValidationFailed, fmt.Sprintf("at most %d points per write", MaxBatch),
			apperr.Detail{Field: "points", Reason: "too_many", Meta: map[string]any{"max": MaxBatch}})
	}
	src, err := s.writeSource(ctx, meta)
	if err != nil {
		return WriteResult{}, err
	}
	if meta.Origin == "" {
		meta.Origin = OriginPrimary
	}
	now := s.clock.Now()
	res := WriteResult{BatchID: uuid.New(), IDs: make([]int64, len(pts))}
	reject := func(i int, key, reason, msg string) {
		res.Rejected = append(res.Rejected, Rejection{Index: i, SeriesKey: key, Reason: reason, Message: msg})
	}

	// Validate.
	seriesCache := map[string]*catalog.Series{}
	seen := map[pointKey]bool{}
	var ok []pending
	for i, p := range pts {
		ser, err := s.lookupSeries(ctx, seriesCache, p.SeriesKey)
		if err != nil {
			return WriteResult{}, err
		}
		if ser == nil {
			reject(i, p.SeriesKey, ReasonUnknownSeries, "series does not exist")
			continue
		}
		if reason, msg := checkPoint(*ser, src, p, now); reason != "" {
			reject(i, p.SeriesKey, reason, msg)
			continue
		}
		k := pointKey{ser.ID, p.ObsDate}
		if seen[k] {
			reject(i, p.SeriesKey, ReasonDuplicatePoint, "the same series and date appear twice in one write")
			continue
		}
		seen[k] = true
		if p.KnownAt.IsZero() {
			p.KnownAt = now
		}
		pd := pending{index: i, series: *ser, p: p}
		if src.Kind == catalog.SourceComputed {
			pd.hash = inputHash(p.DerivVer, p.Inputs)
		}
		ok = append(ok, pd)
	}

	// Skip no-ops.
	ok, err = s.dropUnchanged(ctx, tx, src.ID, ok, &res)
	if err != nil {
		return WriteResult{}, err
	}
	if len(ok) == 0 {
		sortRejections(res.Rejected)
		return res, nil
	}

	// Insert.
	inserted, err := s.insert(ctx, tx, src.ID, meta, res.BatchID, ok)
	if err != nil {
		return WriteResult{}, err
	}
	for _, pd := range ok {
		id, done := inserted[pointKey{pd.series.ID, pd.p.ObsDate}]
		if !done {
			reject(pd.index, pd.p.SeriesKey, ReasonDuplicateKnownAt,
				"a value from this source with the same known_at already exists")
			continue
		}
		res.IDs[pd.index] = id
		res.Written++
	}
	sortRejections(res.Rejected)
	if res.Written == 0 {
		return res, nil
	}
	if err := s.emit(ctx, tx, res.BatchID, meta.Origin, ok, inserted); err != nil {
		return WriteResult{}, err
	}
	return res, nil
}

// writeSource resolves and checks the writing source.
func (s *Store) writeSource(ctx context.Context, meta WriteMeta) (catalog.Source, error) {
	src, err := s.cat.Source(ctx, meta.Source)
	if errors.Is(err, catalog.ErrNotFound) {
		return catalog.Source{}, apperr.New(apperr.ValidationFailed, "unknown source",
			apperr.Detail{Field: "source", Reason: "unknown_source"})
	}
	if err != nil {
		return catalog.Source{}, err
	}
	if !src.Active {
		return catalog.Source{}, apperr.New(apperr.ValidationFailed, "source is inactive",
			apperr.Detail{Field: "source", Reason: "inactive_source"})
	}
	// Human-entered values must be attributable.
	if (src.Kind == catalog.SourceAnalyst || src.Kind == catalog.SourceOverride) && meta.ActorID == nil {
		return catalog.Source{}, apperr.New(apperr.InvalidParams, "analyst and override writes require an actor")
	}
	return src, nil
}

func (s *Store) lookupSeries(ctx context.Context, cache map[string]*catalog.Series, key string) (*catalog.Series, error) {
	if ser, ok := cache[key]; ok {
		return ser, nil
	}
	ser, err := s.cat.Series(ctx, key)
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		cache[key] = nil
		return nil, nil
	case err != nil:
		return nil, err
	}
	cache[key] = &ser
	return &ser, nil
}

// dropUnchanged removes points identical to the current value from the same
// source (computed points: same input hash) and counts them as skipped.
func (s *Store) dropUnchanged(ctx context.Context, tx pgx.Tx, sourceID int64, pts []pending, res *WriteResult) ([]pending, error) {
	if len(pts) == 0 {
		return pts, nil
	}
	ids := make([]int64, len(pts))
	dates := make([]time.Time, len(pts))
	for i, pd := range pts {
		ids[i], dates[i] = pd.series.ID, dateTime(pd.p.ObsDate)
	}
	rows, err := tx.Query(ctx, sqlCurrentBySource, ids, dates, sourceID)
	if err != nil {
		return nil, fmt.Errorf("datapoints: current values: %w", err)
	}
	type current struct {
		v    Value
		hash []byte
	}
	cur := map[pointKey]current{}
	for rows.Next() {
		var (
			sid  int64
			d    time.Time
			c    current
			hash []byte
		)
		if err := rows.Scan(&sid, &d, &c.v.Num, &c.v.Bool, &c.v.Cat, &hash); err != nil {
			rows.Close()
			return nil, fmt.Errorf("datapoints: scan current: %w", err)
		}
		c.hash = hash
		cur[pointKey{sid, civil.DateOf(d)}] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("datapoints: current values: %w", err)
	}
	out := pts[:0]
	for _, pd := range pts {
		c, exists := cur[pointKey{pd.series.ID, pd.p.ObsDate}]
		unchanged := exists && ((pd.hash != nil && string(c.hash) == string(pd.hash)) ||
			(pd.hash == nil && sameValue(c.v, pd.p.Value)))
		if unchanged {
			res.Skipped++
			continue
		}
		out = append(out, pd)
	}
	return out, nil
}

func (s *Store) insert(ctx context.Context, tx pgx.Tx, sourceID int64, meta WriteMeta, batch uuid.UUID, pts []pending) (map[pointKey]int64, error) {
	n := len(pts)
	var (
		seriesIDs = make([]int64, n)
		dates     = make([]time.Time, n)
		known     = make([]time.Time, n)
		nums      = make([]*float64, n)
		bools     = make([]*bool, n)
		cats      = make([]*string, n)
		vers      = make([]*int32, n)
		hashes    = make([][]byte, n)
		inputs    = make([]string, n)
		notes     = make([]string, n)
	)
	for i, pd := range pts {
		p := pd.p
		seriesIDs[i], dates[i], known[i] = pd.series.ID, dateTime(p.ObsDate), p.KnownAt.UTC()
		nums[i], bools[i], cats[i] = p.Value.Num, p.Value.Bool, p.Value.Cat
		hashes[i], notes[i] = pd.hash, p.Note
		if pd.hash != nil {
			inputs[i] = int64Array(p.Inputs)
		}
		if p.DerivVer > 0 {
			v := int32(p.DerivVer) //nolint:gosec // derivation versions are small.
			vers[i] = &v
		}
	}
	rows, err := tx.Query(ctx, sqlInsertPoints, seriesIDs, dates, known, nums, bools, cats, vers, hashes,
		sourceID, batch, meta.JobRunID, meta.ActorID, inputs, notes)
	if err != nil {
		return nil, fmt.Errorf("datapoints: insert: %w", err)
	}
	out := make(map[pointKey]int64, n)
	for rows.Next() {
		var (
			id, sid int64
			d       time.Time
		)
		if err := rows.Scan(&id, &sid, &d); err != nil {
			rows.Close()
			return nil, fmt.Errorf("datapoints: scan insert: %w", err)
		}
		out[pointKey{sid, civil.DateOf(d)}] = id
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("datapoints: insert: %w", err)
	}
	return out, nil
}

// emit writes the batch's outbox event in the same transaction.
func (s *Store) emit(ctx context.Context, tx pgx.Tx, batch uuid.UUID, origin Origin, pts []pending, written map[pointKey]int64) error {
	ev := WrittenEvent{BatchID: batch, Origin: origin}
	seriesSet, layerSet := map[int64]bool{}, map[string]bool{}
	for _, pd := range pts {
		if _, ok := written[pointKey{pd.series.ID, pd.p.ObsDate}]; !ok {
			continue
		}
		seriesSet[pd.series.ID] = true
		layerSet[string(pd.series.Layer)] = true
		if ev.MinDate.IsZero() || pd.p.ObsDate.Before(ev.MinDate) {
			ev.MinDate = pd.p.ObsDate
		}
		if ev.MaxDate.IsZero() || pd.p.ObsDate.After(ev.MaxDate) {
			ev.MaxDate = pd.p.ObsDate
		}
	}
	for id := range seriesSet {
		ev.SeriesIDs = append(ev.SeriesIDs, id)
	}
	for l := range layerSet {
		ev.Layers = append(ev.Layers, l)
	}
	sortEvent(&ev)
	return insertEvent(ctx, tx, ev)
}

// sortEvent makes event payloads deterministic.
func sortEvent(ev *WrittenEvent) {
	sort.Slice(ev.SeriesIDs, func(i, j int) bool { return ev.SeriesIDs[i] < ev.SeriesIDs[j] })
	sort.Strings(ev.Layers)
}

func insertEvent(ctx context.Context, tx pgx.Tx, ev WrittenEvent) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("datapoints: outbox payload: %w", err)
	}
	if _, err := tx.Exec(ctx, sqlInsertOutbox, TopicWritten, payload); err != nil {
		return fmt.Errorf("datapoints: outbox: %w", err)
	}
	return nil
}

func sortRejections(r []Rejection) {
	sort.Slice(r, func(i, j int) bool { return r[i].Index < r[j].Index })
}

// int64Array renders ids as a Postgres array literal ("{1,2,3}"); unnest
// cannot carry ragged per-row arrays, so lineage travels as text and is cast.
func int64Array(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return "{" + strings.Join(parts, ",") + "}"
}
