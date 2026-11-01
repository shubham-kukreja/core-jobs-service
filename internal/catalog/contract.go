// Package catalog owns the data dictionary: entities, sources, series and the
// dependency graph between series. Every other module resolves series keys
// through Catalog; nothing else reads catalog tables.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
)

// Layer is a series' place in the computation pipeline.
type Layer string

const (
	LayerRaw        Layer = "raw"
	LayerJudgment   Layer = "judgment"
	LayerDerived    Layer = "derived"
	LayerWindow     Layer = "window"
	LayerPercentile Layer = "percentile"
	LayerScore      Layer = "score"
	LayerTrigger    Layer = "trigger"
	LayerVeto       Layer = "veto"
)

// Computed reports whether values in the layer come from a derivation.
func (l Layer) Computed() bool {
	switch l {
	case LayerDerived, LayerWindow, LayerPercentile, LayerScore:
		return true
	}
	return false
}

// ValueType is the type every datapoint of a series must have.
type ValueType string

const (
	ValueNum  ValueType = "num"
	ValueBool ValueType = "bool"
	ValueCat  ValueType = "cat"
)

// Frequency is how often a series is observed.
type Frequency string

const (
	FreqDaily     Frequency = "daily"
	FreqWeekly    Frequency = "weekly"
	FreqMonthly   Frequency = "monthly"
	FreqQuarterly Frequency = "quarterly"
	FreqEvent     Frequency = "event"
)

// Align is how a series is read at a coarser frequency.
type Align string

const (
	AlignLast     Align = "last"
	AlignMonthEnd Align = "month_end"
	AlignMean     Align = "mean"
)

// SourceKind classifies where a datapoint came from.
type SourceKind string

const (
	SourceExternalAPI SourceKind = "external_api"
	SourceFileImport  SourceKind = "file_import"
	SourceAnalyst     SourceKind = "analyst"
	SourceOverride    SourceKind = "override"
	SourceComputed    SourceKind = "computed"
)

// Well-known source keys seeded by migrations/seed.
const (
	SourceKeyAnalyst    = "analyst"
	SourceKeyOverride   = "override"
	SourceKeyComputed   = "computed"
	SourceKeyFileImport = "file_import"
)

// Entity is a thing values are about: asset class, segment, sector, index.
type Entity struct {
	ID        int64
	Key       string
	Kind      string
	Name      string
	ParentKey string
	Meta      map[string]any
	Active    bool
}

// Source is the origin of datapoints.
type Source struct {
	ID     int64
	Key    string
	Kind   SourceKind
	Name   string
	Active bool
}

// Validation bounds a series' values. Applied by datapoints.Writer.
type Validation struct {
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
}

// Derivation is a formula that builds a series from other series. Fn names a
// derive.Primitive; Inputs maps the primitive's aliases to series keys.
type Derivation struct {
	Fn      string            `json:"fn"`
	Inputs  map[string]string `json:"inputs"`
	Params  json.RawMessage   `json:"params,omitempty"`
	Version int               `json:"version"`
}

// RegistryMeta is the Data Points screen metadata of a series.
type RegistryMeta struct {
	DisplayName string
	NodeKey     string // 'equity.large'
	SignalKey   string // 'relvalue'
	Group       string // equity | debt | metals | governance | veto
	UpdateType  string // derived | rubric | analyst | constant | governance | veto | raw
	Owner       string
	StaleAfter  time.Duration
	Flags       []string // mock | constant | default
}

// Series is a named quantity tracked over time.
type Series struct {
	ID             int64
	Key            string
	Name           string
	EntityKey      string
	Layer          Layer
	ValueType      ValueType
	Unit           string
	Frequency      Frequency
	Align          Align
	AllowedValues  []string
	Validation     Validation
	Derivation     *Derivation
	MinHistory     *int
	SourcePriority []string
	Tags           []string
	Meta           map[string]any
	Registry       RegistryMeta
	Active         bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// SeriesInput creates or replaces a series definition.
type SeriesInput struct {
	Key            string
	Name           string
	EntityKey      string
	Layer          Layer
	ValueType      ValueType
	Unit           string
	Frequency      Frequency
	Align          Align
	AllowedValues  []string
	Validation     Validation
	Derivation     *Derivation
	MinHistory     *int
	SourcePriority []string
	Tags           []string
	Meta           map[string]any
	Registry       RegistryMeta
}

// SeriesFilter narrows ListSeries.
type SeriesFilter struct {
	Layer     Layer
	EntityKey string
	Tag       string
	Group     string
	Keys      []string // exact keys; supports trailing '*' wildcards ("score.l1.*")
	Page      paging.Request
}

// SaveResult reports what a series save changed.
type SaveResult struct {
	Series Series
	// Rebuild is true when the derivation changed and history must be
	// recomputed; the caller enqueues derive.cascade in rebuild mode in the
	// same transaction.
	Rebuild bool
}

// DepDirection selects upstream (inputs) or downstream (dependents).
type DepDirection string

const (
	DepsUp   DepDirection = "up"
	DepsDown DepDirection = "down"
)

// DepEdge is one edge of the dependency graph.
type DepEdge struct {
	SeriesKey    string
	DependsOnKey string
	Depth        int
}

// DerivationValidator validates a derivation and returns the series keys it
// reads. Implemented by the derive registry and injected into the catalog so
// catalog never imports derive.
type DerivationValidator interface {
	ValidateDerivation(d Derivation) (inputKeys []string, err error)
}

// Reader is the read side used by every module.
type Reader interface {
	Series(ctx context.Context, key string) (Series, error)
	SeriesByIDs(ctx context.Context, ids []int64) (map[int64]Series, error)
	ListSeries(ctx context.Context, f SeriesFilter) (paging.Page[Series], error)
	Entity(ctx context.Context, key string) (Entity, error)
	ListEntities(ctx context.Context, kind string) ([]Entity, error)
	Source(ctx context.Context, key string) (Source, error)
	ListSources(ctx context.Context) ([]Source, error)
	Deps(ctx context.Context, key string, dir DepDirection, maxDepth int) ([]DepEdge, error)
	// Downstream returns every series that depends (transitively) on any of
	// ids, topologically sorted so inputs come before dependents.
	Downstream(ctx context.Context, ids []int64) ([]Series, error)
}

// Writer changes the catalog. Calls take the caller's transaction so the
// audit row and any rebuild job commit atomically with the change.
type Writer interface {
	SaveEntity(ctx context.Context, tx pgx.Tx, e Entity) (Entity, error)
	// SaveSeries validates the definition (including the derivation through
	// DerivationValidator), rejects dependency cycles with ErrCycle, and
	// rebuilds series_deps.
	SaveSeries(ctx context.Context, tx pgx.Tx, in SeriesInput) (SaveResult, error)
	SetSeriesActive(ctx context.Context, tx pgx.Tx, key string, active bool) error
}

// Catalog is the full module surface.
type Catalog interface {
	Reader
	Writer
}

var (
	// ErrNotFound is returned for unknown keys.
	ErrNotFound = errors.New("catalog: not found")
	// ErrCycle is returned when a derivation would create a dependency cycle.
	ErrCycle = errors.New("catalog: dependency cycle")
)
