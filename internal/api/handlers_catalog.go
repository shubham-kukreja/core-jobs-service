package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/auth"
	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
)

// ---------------------------------------------------------------- shapes

type entityJSON struct {
	Key    string         `json:"key"`
	Kind   string         `json:"kind"`
	Name   string         `json:"name"`
	Parent *string        `json:"parent"`
	Meta   map[string]any `json:"meta"`
	Active bool           `json:"active"`
}

func toEntityJSON(e catalog.Entity) entityJSON {
	out := entityJSON{Key: e.Key, Kind: e.Kind, Name: e.Name, Meta: e.Meta, Active: e.Active}
	if e.ParentKey != "" {
		out.Parent = &e.ParentKey
	}
	if out.Meta == nil {
		out.Meta = map[string]any{}
	}
	return out
}

type derivationJSON struct {
	Fn      string            `json:"fn"`
	Inputs  map[string]string `json:"inputs"`
	Params  json.RawMessage   `json:"params,omitempty"`
	Version int               `json:"version,omitempty"`
}

type validationJSON struct {
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
}

type registryJSON struct {
	DisplayName string   `json:"display_name,omitempty"`
	Node        string   `json:"node,omitempty"`
	Signal      string   `json:"signal,omitempty"`
	Group       string   `json:"group,omitempty"`
	UpdateType  string   `json:"update_type,omitempty"`
	Owner       string   `json:"owner,omitempty"`
	StaleAfter  string   `json:"stale_after,omitempty"`
	Flags       []string `json:"flags,omitempty"`
}

type seriesJSON struct {
	Key            string          `json:"key"`
	Name           string          `json:"name"`
	Entity         *string         `json:"entity"`
	Layer          catalog.Layer   `json:"layer"`
	ValueType      string          `json:"value_type"`
	Unit           *string         `json:"unit"`
	Frequency      string          `json:"frequency"`
	Align          string          `json:"align"`
	AllowedValues  []string        `json:"allowed_values"`
	Validation     validationJSON  `json:"validation"`
	Derivation     *derivationJSON `json:"derivation"`
	DerivationVer  *int            `json:"derivation_ver"`
	MinHistory     *int            `json:"min_history"`
	SourcePriority []string        `json:"source_priority"`
	Tags           []string        `json:"tags"`
	Registry       registryJSON    `json:"registry"`
	Active         bool            `json:"active"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

func toSeriesJSON(s catalog.Series) seriesJSON {
	out := seriesJSON{
		Key: s.Key, Name: s.Name, Layer: s.Layer, ValueType: string(s.ValueType), Frequency: string(s.Frequency),
		Align: string(s.Align), AllowedValues: s.AllowedValues, MinHistory: s.MinHistory,
		Validation:     validationJSON{Min: s.Validation.Min, Max: s.Validation.Max},
		SourcePriority: nonNil(s.SourcePriority), Tags: nonNil(s.Tags), Active: s.Active,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
		Registry: registryJSON{
			DisplayName: s.Registry.DisplayName, Node: s.Registry.NodeKey, Signal: s.Registry.SignalKey,
			Group: s.Registry.Group, UpdateType: s.Registry.UpdateType, Owner: s.Registry.Owner,
			StaleAfter: formatStaleAfter(s.Registry.StaleAfter), Flags: s.Registry.Flags,
		},
	}
	if s.EntityKey != "" {
		out.Entity = &s.EntityKey
	}
	if s.Unit != "" {
		out.Unit = &s.Unit
	}
	if d := s.Derivation; d != nil {
		v := d.Version
		out.Derivation = &derivationJSON{Fn: d.Fn, Inputs: d.Inputs, Params: d.Params, Version: d.Version}
		out.DerivationVer = &v
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

type jobRefJSON struct {
	ID     uuid.UUID      `json:"id"`
	Kind   string         `json:"kind"`
	Status jobs.Status    `json:"status"`
	Args   map[string]any `json:"args,omitempty"`
}

type seriesSavedJSON struct {
	Key           string        `json:"key"`
	Layer         catalog.Layer `json:"layer"`
	DerivationVer *int          `json:"derivation_ver"`
	Deps          []string      `json:"deps"`
	RebuildJob    *jobRefJSON   `json:"rebuild_job"`
}

// ---------------------------------------------------------------- entities

func (s *Server) listEntities(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if kind != "" {
		var p problems
		p.oneOf("kind", kind, entityKinds...)
		if err := p.err(); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	es, err := s.catalog.ListEntities(r.Context(), kind)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]entityJSON, 0, len(es))
	for _, e := range es {
		out = append(out, toEntityJSON(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

var entityKinds = []string{"asset_class", "segment", "bucket", "metal", "sector", "index", "macro"}

type entityInput struct {
	Key    string         `json:"key"`
	Kind   string         `json:"kind"`
	Name   string         `json:"name"`
	Parent string         `json:"parent"`
	Meta   map[string]any `json:"meta"`
}

func (s *Server) createEntity(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var in entityInput
	if err := decodeJSON(r, body, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	var p problems
	p.key("key", in.Key)
	p.oneOf("kind", in.Kind, entityKinds...)
	p.text("name", in.Name, 1, 200)
	if in.Parent != "" {
		p.key("parent", in.Parent)
	}
	p.meta("meta", in.Meta)
	if err := p.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.mutate(w, r, body, func(ctx context.Context, tx pgx.Tx) (txResult, error) {
		if _, err := s.catalog.Entity(ctx, in.Key); err == nil {
			return txResult{}, apperr.New(apperr.Conflict, "An entity with this key already exists")
		} else if !errors.Is(err, catalog.ErrNotFound) {
			return txResult{}, err
		}
		e, err := s.catalog.SaveEntity(ctx, tx, catalog.Entity{
			Key: in.Key, Kind: in.Kind, Name: in.Name, ParentKey: in.Parent, Meta: in.Meta, Active: true,
		})
		if err != nil {
			return txResult{}, err
		}
		if err := s.audit.Record(ctx, tx, auditEntry(r, "entity", e.Key, "create", nil, toEntityJSON(e))); err != nil {
			return txResult{}, err
		}
		return txResult{Status: http.StatusCreated, Body: toEntityJSON(e)}, nil
	})
}

type entityPatch struct {
	Name   *string         `json:"name"`
	Parent json.RawMessage `json:"parent"` // absent, null (clear) or a key
	Meta   map[string]any  `json:"meta"`
	Active *bool           `json:"active"`
}

func (s *Server) updateEntity(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	body, err := readBody(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var in entityPatch
	if err := decodeJSON(r, body, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	var p problems
	p.key("key", key)
	if in.Name == nil && in.Parent == nil && in.Meta == nil && in.Active == nil {
		p.add("", "empty_patch")
	}
	if in.Name != nil {
		p.text("name", *in.Name, 1, 200)
	}
	var parent *string
	if in.Parent != nil && !bytes.Equal(bytes.TrimSpace(in.Parent), []byte("null")) {
		var k string
		if json.Unmarshal(in.Parent, &k) != nil {
			p.add("parent", "wrong_type")
		} else {
			p.key("parent", k)
			parent = &k
		}
	}
	p.meta("meta", in.Meta)
	if err := p.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.mutate(w, r, body, func(ctx context.Context, tx pgx.Tx) (txResult, error) {
		before, err := s.catalog.Entity(ctx, key)
		if err != nil {
			return txResult{}, err
		}
		next := before
		if in.Name != nil {
			next.Name = *in.Name
		}
		if in.Parent != nil {
			next.ParentKey = ""
			if parent != nil {
				next.ParentKey = *parent
			}
		}
		if in.Meta != nil {
			next.Meta = in.Meta
		}
		if in.Active != nil {
			next.Active = *in.Active
		}
		saved, err := s.catalog.SaveEntity(ctx, tx, next)
		if err != nil {
			return txResult{}, err
		}
		if err := s.audit.Record(ctx, tx, auditEntry(r, "entity", key, "update", toEntityJSON(before), toEntityJSON(saved))); err != nil {
			return txResult{}, err
		}
		return txResult{Status: http.StatusOK, Body: toEntityJSON(saved)}, nil
	})
}

// ---------------------------------------------------------------- sources

type sourceJSON struct {
	Key                string `json:"key"`
	Kind               string `json:"kind"`
	Name               string `json:"name"`
	Active             bool   `json:"active"`
	ConnectorAvailable bool   `json:"connector_available"`
}

func (s *Server) listSources(w http.ResponseWriter, r *http.Request) {
	srcs, err := s.catalog.ListSources(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]sourceJSON, 0, len(srcs))
	for _, src := range srcs {
		available := false
		if s.connectors != nil {
			_, available = s.connectors.Get(src.Key)
		}
		// Connector settings and credential references are deliberately omitted.
		out = append(out, sourceJSON{Key: src.Key, Kind: string(src.Kind), Name: src.Name, Active: src.Active, ConnectorAvailable: available})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// ---------------------------------------------------------------- series

var (
	layers     = []string{"raw", "judgment", "derived", "window", "percentile", "score", "trigger", "veto"}
	groups     = []string{"equity", "debt", "metals", "governance", "veto"}
	valueTypes = []string{"num", "bool", "cat"}
	freqs      = []string{"daily", "weekly", "monthly", "quarterly", "event"}
	aligns     = []string{"last", "month_end", "mean"}
	updates    = []string{"derived", "rubric", "analyst", "constant", "governance", "veto", "raw"}
	flagValues = []string{"mock", "constant", "default"}
)

func (s *Server) listSeries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var p problems
	f := catalog.SeriesFilter{Layer: catalog.Layer(q.Get("layer")), EntityKey: q.Get("entity"), Tag: q.Get("tag"), Group: q.Get("group")}
	if f.Layer != "" {
		p.oneOf("layer", string(f.Layer), layers...)
	}
	if f.EntityKey != "" {
		p.key("entity", f.EntityKey)
	}
	if f.Tag != "" && !tagRE.MatchString(f.Tag) {
		p.add("tag", "invalid_format")
	}
	if f.Group != "" {
		p.oneOf("group", f.Group, groups...)
	}
	if err := p.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	const scope = "GET /v1/series"
	page, err := queryPage(r, s, scope)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	f.Page = page
	res, err := s.catalog.ListSeries(r.Context(), f)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]seriesJSON, 0, len(res.Items))
	for _, it := range res.Items {
		out = append(out, toSeriesJSON(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "next_cursor": s.cursors.Encode(scope, res.Next)})
}

func (s *Server) getSeries(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if !keyRE.MatchString(key) {
		s.writeError(w, r, apperr.New(apperr.NotFound, "Not found"))
		return
	}
	ser, err := s.catalog.Series(r.Context(), key)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toSeriesJSON(ser))
}

// seriesFields are the writable fields shared by create and patch.
type seriesFields struct {
	Name           *string         `json:"name"`
	Entity         *string         `json:"entity"`
	Unit           *string         `json:"unit"`
	Frequency      *string         `json:"frequency"`
	Align          *string         `json:"align"`
	AllowedValues  []string        `json:"allowed_values"`
	Validation     *validationJSON `json:"validation"`
	Derivation     *derivationJSON `json:"derivation"`
	MinHistory     *int            `json:"min_history"`
	SourcePriority []string        `json:"source_priority"`
	Tags           []string        `json:"tags"`
	Meta           map[string]any  `json:"meta"`
	Registry       *registryJSON   `json:"registry"`
}

type seriesCreate struct {
	seriesFields
	Key       string `json:"key"`
	Layer     string `json:"layer"`
	ValueType string `json:"value_type"`
}

type seriesPatch struct {
	seriesFields
	Active *bool `json:"active"`
}

func (f *seriesFields) empty() bool {
	return f.Name == nil && f.Entity == nil && f.Unit == nil && f.Frequency == nil && f.Align == nil &&
		f.AllowedValues == nil && f.Validation == nil && f.Derivation == nil && f.MinHistory == nil &&
		f.SourcePriority == nil && f.Tags == nil && f.Meta == nil && f.Registry == nil
}

func (f *seriesFields) check(p *problems) {
	if f.Name != nil {
		p.text("name", *f.Name, 1, 200)
	}
	if f.Entity != nil {
		p.key("entity", *f.Entity)
	}
	if f.Unit != nil {
		p.text("unit", *f.Unit, 0, 32)
	}
	if f.Frequency != nil {
		p.oneOf("frequency", *f.Frequency, freqs...)
	}
	if f.Align != nil {
		p.oneOf("align", *f.Align, aligns...)
	}
	if f.AllowedValues != nil {
		if len(f.AllowedValues) < 2 || len(f.AllowedValues) > 50 || !unique(f.AllowedValues) {
			p.add("allowed_values", "invalid_length")
		}
		for _, v := range f.AllowedValues {
			if !categoryRE.MatchString(v) {
				p.add("allowed_values", "invalid_format")
			}
		}
	}
	if v := f.Validation; v != nil && v.Min != nil && v.Max != nil && *v.Min > *v.Max {
		p.add("validation", "min_exceeds_max")
	}
	if d := f.Derivation; d != nil {
		if !fnRE.MatchString(d.Fn) {
			p.add("derivation.fn", "invalid_format")
		}
		if len(d.Inputs) > 32 {
			p.add("derivation.inputs", "too_many_properties")
		}
		for alias, k := range d.Inputs {
			if !aliasRE.MatchString(alias) || !keyRE.MatchString(k) {
				p.add("derivation.inputs."+alias, "invalid_key")
			}
		}
		if d.Version != 0 {
			p.add("derivation.version", "read_only")
		}
		if len(d.Params) > 0 && !bytes.HasPrefix(bytes.TrimSpace(d.Params), []byte("{")) {
			p.add("derivation.params", "wrong_type")
		}
	}
	if f.MinHistory != nil && (*f.MinHistory < 1 || *f.MinHistory > 10000) {
		p.add("min_history", "out_of_range")
	}
	if f.SourcePriority != nil {
		if len(f.SourcePriority) < 1 || len(f.SourcePriority) > 20 || !unique(f.SourcePriority) {
			p.add("source_priority", "invalid_length")
		}
		for _, k := range f.SourcePriority {
			p.key("source_priority", k)
		}
	}
	if f.Tags != nil {
		if len(f.Tags) > 20 || !unique(f.Tags) {
			p.add("tags", "invalid_length")
		}
		for _, t := range f.Tags {
			if !tagRE.MatchString(t) {
				p.add("tags", "invalid_format")
			}
		}
	}
	p.meta("meta", f.Meta)
	if g := f.Registry; g != nil {
		p.text("registry.display_name", g.DisplayName, 0, 200)
		if g.Node != "" {
			p.key("registry.node", g.Node)
		}
		if g.Signal != "" {
			p.key("registry.signal", g.Signal)
		}
		if g.Group != "" {
			p.oneOf("registry.group", g.Group, groups...)
		}
		if g.UpdateType != "" {
			p.oneOf("registry.update_type", g.UpdateType, updates...)
		}
		p.text("registry.owner", g.Owner, 0, 64)
		if g.StaleAfter != "" {
			if _, ok := parseStaleAfter(g.StaleAfter); !ok {
				p.add("registry.stale_after", "invalid_format")
			}
		}
		if len(g.Flags) > 3 || !unique(g.Flags) {
			p.add("registry.flags", "invalid_length")
		}
		for _, fl := range g.Flags {
			p.oneOf("registry.flags", fl, flagValues...)
		}
	}
}

// apply writes the provided fields onto in.
func (f *seriesFields) apply(in *catalog.SeriesInput) {
	if f.Name != nil {
		in.Name = *f.Name
	}
	if f.Entity != nil {
		in.EntityKey = *f.Entity
	}
	if f.Unit != nil {
		in.Unit = *f.Unit
	}
	if f.Frequency != nil {
		in.Frequency = catalog.Frequency(*f.Frequency)
	}
	if f.Align != nil {
		in.Align = catalog.Align(*f.Align)
	}
	if f.AllowedValues != nil {
		in.AllowedValues = f.AllowedValues
	}
	if f.Validation != nil {
		in.Validation = catalog.Validation{Min: f.Validation.Min, Max: f.Validation.Max}
	}
	if f.Derivation != nil {
		in.Derivation = &catalog.Derivation{Fn: f.Derivation.Fn, Inputs: f.Derivation.Inputs, Params: f.Derivation.Params}
	}
	if f.MinHistory != nil {
		in.MinHistory = f.MinHistory
	}
	if f.SourcePriority != nil {
		in.SourcePriority = f.SourcePriority
	}
	if f.Tags != nil {
		in.Tags = f.Tags
	}
	if f.Meta != nil {
		in.Meta = f.Meta
	}
	if g := f.Registry; g != nil {
		stale, _ := parseStaleAfter(g.StaleAfter)
		in.Registry = catalog.RegistryMeta{
			DisplayName: g.DisplayName, NodeKey: g.Node, SignalKey: g.Signal, Group: g.Group,
			UpdateType: g.UpdateType, Owner: g.Owner, StaleAfter: stale, Flags: g.Flags,
		}
	}
}

func (s *Server) createSeries(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var in seriesCreate
	if err := decodeJSON(r, body, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	var p problems
	p.key("key", in.Key)
	p.oneOf("layer", in.Layer, layers...)
	p.oneOf("value_type", in.ValueType, valueTypes...)
	if in.Name == nil {
		p.add("name", "required")
	}
	if in.Frequency == nil {
		p.add("frequency", "required")
	}
	in.check(&p)
	if err := p.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	si := catalog.SeriesInput{Key: in.Key, Layer: catalog.Layer(in.Layer), ValueType: catalog.ValueType(in.ValueType)}
	in.apply(&si)
	s.mutate(w, r, body, func(ctx context.Context, tx pgx.Tx) (txResult, error) {
		if _, err := s.catalog.Series(ctx, in.Key); err == nil {
			return txResult{}, apperr.New(apperr.Conflict, "A series with this key already exists")
		} else if !errors.Is(err, catalog.ErrNotFound) {
			return txResult{}, err
		}
		return s.saveSeries(ctx, r, tx, si, nil, "create", http.StatusCreated)
	})
}

func (s *Server) updateSeries(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	body, err := readBody(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var in seriesPatch
	if err := decodeJSON(r, body, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	var p problems
	p.key("key", key)
	in.check(&p)
	if in.empty() && in.Active == nil {
		p.add("", "empty_patch")
	}
	if err := p.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.mutate(w, r, body, func(ctx context.Context, tx pgx.Tx) (txResult, error) {
		cur, err := s.catalog.Series(ctx, key)
		if err != nil {
			return txResult{}, err
		}
		si := seriesInputFrom(cur)
		in.apply(&si)
		if in.Active != nil {
			if err := s.catalog.SetSeriesActive(ctx, tx, key, *in.Active); err != nil {
				return txResult{}, err
			}
		}
		return s.saveSeries(ctx, r, tx, si, &cur, "update", http.StatusOK)
	})
}

// saveSeries saves, enqueues a history rebuild when the derivation changed,
// and audits, all in tx.
func (s *Server) saveSeries(ctx context.Context, r *http.Request, tx pgx.Tx, si catalog.SeriesInput, before *catalog.Series, action string, status int) (txResult, error) {
	p := mustPrincipal(ctx)
	res, err := s.catalog.SaveSeries(ctx, tx, si)
	if err != nil {
		return txResult{}, err
	}
	out := seriesSavedJSON{Key: res.Series.Key, Layer: res.Series.Layer, Deps: []string{}}
	if d := res.Series.Derivation; d != nil {
		v := d.Version
		out.DerivationVer = &v
		for _, k := range d.Inputs {
			out.Deps = append(out.Deps, k)
		}
		sort.Strings(out.Deps)
		out.Deps = dedupe(out.Deps)
	}
	if res.Rebuild {
		keys := []string{res.Series.Key}
		ref, err := s.rebuilds.EnqueueRebuild(ctx, tx, keys, p.UserID)
		if err != nil {
			return txResult{}, err
		}
		out.RebuildJob = &jobRefJSON{
			ID: ref.UUID, Kind: "derive.cascade", Status: ref.Status,
			Args: map[string]any{"mode": "rebuild", "series": keys},
		}
	}
	var prev any
	if before != nil {
		prev = toSeriesJSON(*before)
	}
	if err := s.audit.Record(ctx, tx, auditEntry(r, "series", res.Series.Key, action, prev, toSeriesJSON(res.Series))); err != nil {
		return txResult{}, err
	}
	return txResult{Status: status, Body: out}, nil
}

func seriesInputFrom(c catalog.Series) catalog.SeriesInput {
	return catalog.SeriesInput{
		Key: c.Key, Name: c.Name, EntityKey: c.EntityKey, Layer: c.Layer, ValueType: c.ValueType, Unit: c.Unit,
		Frequency: c.Frequency, Align: c.Align, AllowedValues: c.AllowedValues, Validation: c.Validation,
		Derivation: c.Derivation, MinHistory: c.MinHistory, SourcePriority: c.SourcePriority, Tags: c.Tags,
		Meta: c.Meta, Registry: c.Registry,
	}
}

type depJSON struct {
	Series    string `json:"series"`
	DependsOn string `json:"depends_on"`
	Depth     int    `json:"depth"`
}

func (s *Server) seriesDeps(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	dir := r.URL.Query().Get("direction")
	if dir == "" {
		dir = string(catalog.DepsUp)
	}
	var p problems
	p.key("key", key)
	p.oneOf("direction", dir, string(catalog.DepsUp), string(catalog.DepsDown))
	if err := p.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	depth, err := queryInt(r, "depth", 3, 1, 10)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if _, err := s.catalog.Series(r.Context(), key); err != nil {
		s.writeError(w, r, err)
		return
	}
	edges, err := s.catalog.Deps(r.Context(), key, catalog.DepDirection(dir), depth)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]depJSON, 0, len(edges))
	for _, e := range edges {
		out = append(out, depJSON{Series: e.SeriesKey, DependsOn: e.DependsOnKey, Depth: e.Depth})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func unique(xs []string) bool {
	seen := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		if _, dup := seen[x]; dup {
			return false
		}
		seen[x] = struct{}{}
	}
	return true
}

func dedupe(sorted []string) []string {
	out := sorted[:0]
	for i, x := range sorted {
		if i == 0 || x != sorted[i-1] {
			out = append(out, x)
		}
	}
	return out
}

// isAdmin reports whether the caller may perform admin-only variants.
func isAdmin(p auth.Principal) bool { return p.Role.AtLeast(auth.RoleAdmin) }
