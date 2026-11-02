package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog/internal/db"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
)

// DefaultSourcePriority is used when a series does not set one. Entries are
// source keys or source kinds (see the datapoints reader).
var DefaultSourcePriority = []string{"override", "analyst", "external_api", "file_import", "computed"}

// Store implements Catalog on Postgres.
type Store struct {
	pool      *pgxpool.Pool
	q         *db.Queries
	validator DerivationValidator
}

var _ Catalog = (*Store)(nil)

// New returns a catalog store. v validates derivations on save and must not be nil.
func New(pool *pgxpool.Pool, v DerivationValidator) *Store {
	if v == nil {
		panic("catalog: nil DerivationValidator")
	}
	return &Store{pool: pool, q: db.New(pool), validator: v}
}

func notFound(what, key string) error {
	return apperr.Wrap(ErrNotFound, apperr.NotFound, fmt.Sprintf("%s %q not found", what, key))
}

// ---------------------------------------------------------------------------
// Reader

func (s *Store) Series(ctx context.Context, key string) (Series, error) {
	return getSeries(ctx, s.q, key)
}

func getSeries(ctx context.Context, q *db.Queries, key string) (Series, error) {
	r, err := q.GetSeries(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return Series{}, notFound("series", key)
	}
	if err != nil {
		return Series{}, fmt.Errorf("catalog: get series: %w", err)
	}
	return seriesFromRow(r.CatalogSeries, r.EntityKey)
}

func (s *Store) SeriesByIDs(ctx context.Context, ids []int64) (map[int64]Series, error) {
	out := make(map[int64]Series, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.q.GetSeriesByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("catalog: series by ids: %w", err)
	}
	for _, r := range rows {
		ser, err := seriesFromRow(r.CatalogSeries, r.EntityKey)
		if err != nil {
			return nil, err
		}
		out[ser.ID] = ser
	}
	return out, nil
}

func (s *Store) ListSeries(ctx context.Context, f SeriesFilter) (paging.Page[Series], error) {
	page := f.Page.Normalize()
	keys, patterns, err := splitKeyPatterns(f.Keys)
	if err != nil {
		return paging.Page[Series]{}, err
	}
	rows, err := s.q.ListSeries(ctx, db.ListSeriesParams{
		Layer:         optional(string(f.Layer)),
		EntityKey:     optional(f.EntityKey),
		Tag:           optional(f.Tag),
		RegistryGroup: optional(f.Group),
		Keys:          keys,
		Patterns:      patterns,
		After:         page.After,
		Lim:           int32(page.Limit + 1), //nolint:gosec // bounded by paging.MaxLimit.
	})
	if err != nil {
		return paging.Page[Series]{}, fmt.Errorf("catalog: list series: %w", err)
	}
	out := paging.Page[Series]{Items: make([]Series, 0, len(rows))}
	for i, r := range rows {
		if i == page.Limit {
			out.Next = out.Items[len(out.Items)-1].Key
			break
		}
		ser, err := seriesFromRow(r.CatalogSeries, r.EntityKey)
		if err != nil {
			return paging.Page[Series]{}, err
		}
		out.Items = append(out.Items, ser)
	}
	return out, nil
}

// splitKeyPatterns separates exact keys from trailing-* wildcards and turns
// wildcards into escaped LIKE prefix patterns. A '*' anywhere else is invalid.
func splitKeyPatterns(in []string) (keys, patterns []string, err error) {
	keys, patterns = []string{}, []string{}
	for _, k := range in {
		prefix, wild := strings.CutSuffix(k, "*")
		if strings.Contains(prefix, "*") || (!wild && !ValidKey(k)) || (wild && prefix == "") {
			return nil, nil, apperr.New(apperr.ValidationFailed, "invalid series key filter",
				apperr.Detail{Field: "keys", Reason: "invalid_pattern", Meta: map[string]any{"value": k}})
		}
		if wild {
			patterns = append(patterns, escapeLike(prefix)+"%")
		} else {
			keys = append(keys, k)
		}
	}
	return keys, patterns, nil
}

// escapeLike escapes LIKE metacharacters (backslash is Postgres' default escape).
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (s *Store) Entity(ctx context.Context, key string) (Entity, error) {
	r, err := s.q.GetEntity(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return Entity{}, notFound("entity", key)
	}
	if err != nil {
		return Entity{}, fmt.Errorf("catalog: get entity: %w", err)
	}
	return entityFrom(r.ID, r.Key, r.Kind, r.Name, r.Meta, r.Active, r.ParentKey)
}

func (s *Store) ListEntities(ctx context.Context, kind string) ([]Entity, error) {
	rows, err := s.q.ListEntities(ctx, optional(kind))
	if err != nil {
		return nil, fmt.Errorf("catalog: list entities: %w", err)
	}
	out := make([]Entity, 0, len(rows))
	for _, r := range rows {
		e, err := entityFrom(r.ID, r.Key, r.Kind, r.Name, r.Meta, r.Active, r.ParentKey)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *Store) Source(ctx context.Context, key string) (Source, error) {
	r, err := s.q.GetSource(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return Source{}, notFound("source", key)
	}
	if err != nil {
		return Source{}, fmt.Errorf("catalog: get source: %w", err)
	}
	return Source{ID: r.ID, Key: r.Key, Kind: SourceKind(r.Kind), Name: r.Name, Active: r.Active}, nil
}

func (s *Store) ListSources(ctx context.Context) ([]Source, error) {
	rows, err := s.q.ListSources(ctx)
	if err != nil {
		return nil, fmt.Errorf("catalog: list sources: %w", err)
	}
	out := make([]Source, 0, len(rows))
	for _, r := range rows {
		out = append(out, Source{ID: r.ID, Key: r.Key, Kind: SourceKind(r.Kind), Name: r.Name, Active: r.Active})
	}
	return out, nil
}

func (s *Store) Deps(ctx context.Context, key string, dir DepDirection, maxDepth int) ([]DepEdge, error) {
	if dir != DepsUp && dir != DepsDown {
		return nil, apperr.New(apperr.ValidationFailed, "direction must be up or down")
	}
	root, err := s.Series(ctx, key)
	if err != nil {
		return nil, err
	}
	g, err := loadGraph(ctx, s.q)
	if err != nil {
		return nil, err
	}
	keys, err := s.seriesKeys(ctx)
	if err != nil {
		return nil, err
	}
	walked := g.walk(root.ID, dir, maxDepth)
	out := make([]DepEdge, 0, len(walked))
	for _, w := range walked {
		out = append(out, DepEdge{SeriesKey: keys[w.series], DependsOnKey: keys[w.dependsOn], Depth: w.depth})
	}
	return out, nil
}

func (s *Store) Downstream(ctx context.Context, ids []int64) ([]Series, error) {
	g, err := loadGraph(ctx, s.q)
	if err != nil {
		return nil, err
	}
	order := g.downstream(ids)
	byID, err := s.SeriesByIDs(ctx, order)
	if err != nil {
		return nil, err
	}
	out := make([]Series, 0, len(order))
	for _, id := range order {
		if ser, ok := byID[id]; ok {
			out = append(out, ser)
		}
	}
	return out, nil
}

func (s *Store) seriesKeys(ctx context.Context) (map[int64]string, error) {
	rows, err := s.q.AllSeriesKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("catalog: series keys: %w", err)
	}
	m := make(map[int64]string, len(rows))
	for _, r := range rows {
		m[r.ID] = r.Key
	}
	return m, nil
}

func loadGraph(ctx context.Context, q *db.Queries) (*graph, error) {
	rows, err := q.AllDeps(ctx)
	if err != nil {
		return nil, fmt.Errorf("catalog: load deps: %w", err)
	}
	edges := make([]edge, len(rows))
	for i, r := range rows {
		edges[i] = edge{series: r.SeriesID, dependsOn: r.DependsOnID}
	}
	return newGraph(edges), nil
}

// ---------------------------------------------------------------------------
// Writer

func (s *Store) SaveEntity(ctx context.Context, tx pgx.Tx, e Entity) (Entity, error) {
	if err := validateEntity(e); err != nil {
		return Entity{}, err
	}
	q := db.New(tx)
	var parentID *int64
	if e.ParentKey != "" {
		id, err := q.EntityID(ctx, e.ParentKey)
		if errors.Is(err, pgx.ErrNoRows) {
			return Entity{}, apperr.New(apperr.ValidationFailed, "entity is invalid",
				apperr.Detail{Field: "parent", Reason: "unknown_entity"})
		}
		if err != nil {
			return Entity{}, fmt.Errorf("catalog: parent lookup: %w", err)
		}
		parentID = &id
	}
	meta, err := marshalObject(e.Meta)
	if err != nil {
		return Entity{}, err
	}
	id, err := q.UpsertEntity(ctx, db.UpsertEntityParams{
		Key: e.Key, Kind: e.Kind, Name: e.Name, ParentID: parentID, Meta: meta, Active: e.Active,
	})
	if err != nil {
		return Entity{}, fmt.Errorf("catalog: save entity: %w", err)
	}
	e.ID = id
	return e, nil
}

func (s *Store) SaveSeries(ctx context.Context, tx pgx.Tx, in SeriesInput) (SaveResult, error) {
	if err := normalizeSeries(&in); err != nil {
		return SaveResult{}, err
	}
	var inputKeys []string
	if in.Derivation != nil {
		keys, err := s.validator.ValidateDerivation(*in.Derivation)
		if err != nil {
			if _, ok := apperr.As(err); ok {
				return SaveResult{}, err
			}
			return SaveResult{}, apperr.Wrap(err, apperr.ValidationFailed, "derivation is invalid")
		}
		inputKeys = uniqueSorted(append(keys, mapValues(in.Derivation.Inputs)...))
	}

	q := db.New(tx)
	if err := q.LockDepsGraph(ctx); err != nil {
		return SaveResult{}, fmt.Errorf("catalog: lock deps: %w", err)
	}

	var p problems
	var entityID *int64
	if in.EntityKey != "" {
		id, err := q.EntityID(ctx, in.EntityKey)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			p.add("entity", "unknown_entity", nil)
		case err != nil:
			return SaveResult{}, fmt.Errorf("catalog: entity lookup: %w", err)
		default:
			entityID = &id
		}
	}
	if len(in.SourcePriority) == 0 {
		in.SourcePriority = DefaultSourcePriority
	}
	srcs, err := q.ListSources(ctx)
	if err != nil {
		return SaveResult{}, fmt.Errorf("catalog: sources: %w", err)
	}
	known := map[string]bool{}
	for _, src := range srcs {
		known[src.Key], known[src.Kind] = true, true
	}
	for _, sp := range in.SourcePriority {
		if !known[sp] && !kindNames[sp] {
			p.add("source_priority", "unknown_source", map[string]any{"value": sp})
		}
	}
	inputIDs, missing, err := seriesIDs(ctx, q, inputKeys)
	if err != nil {
		return SaveResult{}, err
	}
	for _, k := range missing {
		p.add("derivation.inputs", "unknown_series", map[string]any{"series": k})
	}
	if err := p.err("series definition is invalid"); err != nil {
		return SaveResult{}, err
	}

	ver, rebuild, err := nextDerivationVersion(ctx, q, in.Key, in.Derivation)
	if err != nil {
		return SaveResult{}, err
	}
	params, err := upsertParams(in, entityID, ver)
	if err != nil {
		return SaveResult{}, err
	}
	id, err := q.UpsertSeries(ctx, params)
	if err != nil {
		return SaveResult{}, fmt.Errorf("catalog: save series: %w", err)
	}

	g, err := loadGraph(ctx, q)
	if err != nil {
		return SaveResult{}, err
	}
	if g.wouldCycle(id, inputIDs) {
		return SaveResult{}, apperr.Wrap(ErrCycle, apperr.Conflict,
			fmt.Sprintf("derivation of %q would create a dependency cycle", in.Key))
	}
	if err := q.DeleteDeps(ctx, id); err != nil {
		return SaveResult{}, fmt.Errorf("catalog: delete deps: %w", err)
	}
	if len(inputIDs) > 0 {
		if err := q.InsertDeps(ctx, db.InsertDepsParams{SeriesID: id, DependsOn: inputIDs}); err != nil {
			return SaveResult{}, fmt.Errorf("catalog: insert deps: %w", err)
		}
	}
	saved, err := getSeries(ctx, q, in.Key)
	if err != nil {
		return SaveResult{}, err
	}
	return SaveResult{Series: saved, Rebuild: rebuild}, nil
}

// kindNames lets source_priority name source kinds as well as keys.
var kindNames = set("external_api", "file_import", "analyst", "override", "computed")

func (s *Store) SetSeriesActive(ctx context.Context, tx pgx.Tx, key string, active bool) error {
	n, err := db.New(tx).SetSeriesActive(ctx, db.SetSeriesActiveParams{Key: key, Active: active})
	if err != nil {
		return fmt.Errorf("catalog: set active: %w", err)
	}
	if n == 0 {
		return notFound("series", key)
	}
	return nil
}

// nextDerivationVersion keeps the stored version when the derivation is
// unchanged and bumps it (requesting a rebuild) when it changes.
func nextDerivationVersion(ctx context.Context, q *db.Queries, key string, d *Derivation) (*int32, bool, error) {
	cur, err := q.GetSeriesForUpdate(ctx, key)
	exists := true
	if errors.Is(err, pgx.ErrNoRows) {
		exists = false
	} else if err != nil {
		return nil, false, fmt.Errorf("catalog: lock series: %w", err)
	}
	if d == nil {
		return nil, false, nil
	}
	if exists && cur.Derivation != nil && cur.DerivationVer != nil {
		var old Derivation
		if err := json.Unmarshal(cur.Derivation, &old); err != nil {
			return nil, false, fmt.Errorf("catalog: stored derivation: %w", err)
		}
		same, err := sameDerivation(old, *d)
		if err != nil {
			return nil, false, err
		}
		if same {
			return cur.DerivationVer, false, nil
		}
		v := *cur.DerivationVer + 1
		return &v, true, nil
	}
	v := int32(1)
	return &v, true, nil
}

// sameDerivation compares derivations ignoring Version and JSON formatting.
func sameDerivation(a, b Derivation) (bool, error) {
	a.Version, b.Version = 0, 0
	ca, err := canonical(a)
	if err != nil {
		return false, err
	}
	cb, err := canonical(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ca, cb), nil
}

// canonical marshals v with sorted keys at every level (encoding/json sorts
// map keys; RawMessage params are re-decoded so their formatting is ignored).
func canonical(d Derivation) ([]byte, error) {
	var params any
	if len(d.Params) > 0 {
		if err := json.Unmarshal(d.Params, &params); err != nil {
			return nil, apperr.Wrap(err, apperr.ValidationFailed, "derivation params are not valid JSON")
		}
	}
	return json.Marshal(struct {
		Fn     string            `json:"fn"`
		Inputs map[string]string `json:"inputs"`
		Params any               `json:"params"`
	}{d.Fn, d.Inputs, params})
}

func seriesIDs(ctx context.Context, q *db.Queries, keys []string) (ids []int64, missing []string, err error) {
	if len(keys) == 0 {
		return nil, nil, nil
	}
	rows, err := q.SeriesIDsByKeys(ctx, keys)
	if err != nil {
		return nil, nil, fmt.Errorf("catalog: resolve inputs: %w", err)
	}
	found := map[string]int64{}
	for _, r := range rows {
		found[r.Key] = r.ID
	}
	for _, k := range keys {
		if id, ok := found[k]; ok {
			ids = append(ids, id)
		} else {
			missing = append(missing, k)
		}
	}
	return ids, missing, nil
}

// ---------------------------------------------------------------------------
// Row mapping

func upsertParams(in SeriesInput, entityID *int64, ver *int32) (db.UpsertSeriesParams, error) {
	validation, err := json.Marshal(in.Validation)
	if err != nil {
		return db.UpsertSeriesParams{}, err
	}
	meta, err := marshalObject(in.Meta)
	if err != nil {
		return db.UpsertSeriesParams{}, err
	}
	var deriv []byte
	if in.Derivation != nil {
		d := *in.Derivation
		d.Version = int(*ver)
		if d.Inputs == nil {
			d.Inputs = map[string]string{}
		}
		if deriv, err = json.Marshal(d); err != nil {
			return db.UpsertSeriesParams{}, err
		}
	}
	var minHistory *int32
	if in.MinHistory != nil {
		v := int32(*in.MinHistory) //nolint:gosec // validated positive; series history counts are small.
		minHistory = &v
	}
	r := in.Registry
	return db.UpsertSeriesParams{
		Key: in.Key, Name: in.Name, EntityID: entityID, Layer: string(in.Layer), ValueType: string(in.ValueType),
		Unit: optional(in.Unit), Frequency: string(in.Frequency), Align: string(in.Align),
		AllowedValues: nilIfEmpty(in.AllowedValues), Validation: validation, Derivation: deriv, DerivationVer: ver,
		MinHistory: minHistory, SourcePriority: in.SourcePriority, Tags: nonNil(in.Tags), Meta: meta,
		DisplayName: optional(r.DisplayName), NodeKey: optional(r.NodeKey), SignalKey: optional(r.SignalKey),
		RegistryGroup: optional(r.Group), UpdateType: optional(r.UpdateType), Owner: optional(r.Owner),
		StaleAfter: interval(r.StaleAfter), Flags: nonNil(r.Flags),
	}, nil
}

func seriesFromRow(r db.CatalogSeries, entityKey *string) (Series, error) {
	s := Series{
		ID: r.ID, Key: r.Key, Name: r.Name, EntityKey: deref(entityKey), Layer: Layer(r.Layer),
		ValueType: ValueType(r.ValueType), Unit: deref(r.Unit), Frequency: Frequency(r.Frequency), Align: Align(r.Align),
		AllowedValues: r.AllowedValues, SourcePriority: r.SourcePriority, Tags: r.Tags, Active: r.Active,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		Registry: RegistryMeta{
			DisplayName: deref(r.DisplayName), NodeKey: deref(r.NodeKey), SignalKey: deref(r.SignalKey),
			Group: deref(r.RegistryGroup), UpdateType: deref(r.UpdateType), Owner: deref(r.Owner),
			StaleAfter: duration(r.StaleAfter), Flags: r.Flags,
		},
	}
	if r.MinHistory != nil {
		v := int(*r.MinHistory)
		s.MinHistory = &v
	}
	if err := json.Unmarshal(r.Validation, &s.Validation); err != nil {
		return Series{}, fmt.Errorf("catalog: %s validation: %w", r.Key, err)
	}
	if err := json.Unmarshal(r.Meta, &s.Meta); err != nil {
		return Series{}, fmt.Errorf("catalog: %s meta: %w", r.Key, err)
	}
	if r.Derivation != nil {
		var d Derivation
		if err := json.Unmarshal(r.Derivation, &d); err != nil {
			return Series{}, fmt.Errorf("catalog: %s derivation: %w", r.Key, err)
		}
		if r.DerivationVer != nil {
			d.Version = int(*r.DerivationVer)
		}
		s.Derivation = &d
	}
	return s, nil
}

func entityFrom(id int64, key, kind, name string, meta []byte, active bool, parent *string) (Entity, error) {
	e := Entity{ID: id, Key: key, Kind: kind, Name: name, Active: active, ParentKey: deref(parent)}
	if err := json.Unmarshal(meta, &e.Meta); err != nil {
		return Entity{}, fmt.Errorf("catalog: entity %s meta: %w", key, err)
	}
	return e, nil
}

func marshalObject(m map[string]any) ([]byte, error) {
	if m == nil {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, apperr.Wrap(err, apperr.ValidationFailed, "meta is not valid JSON")
	}
	return b, nil
}

func interval(d time.Duration) pgtype.Interval {
	if d <= 0 {
		return pgtype.Interval{}
	}
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}

func duration(i pgtype.Interval) time.Duration {
	if !i.Valid {
		return 0
	}
	const day = 24 * time.Hour
	return time.Duration(i.Months)*30*day + time.Duration(i.Days)*day + time.Duration(i.Microseconds)*time.Microsecond
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func uniqueSorted(s []string) []string {
	sort.Strings(s)
	return slices.Compact(s)
}
