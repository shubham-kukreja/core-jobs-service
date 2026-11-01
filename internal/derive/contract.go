// Package derive computes derived, window, percentile, score and trigger
// series. Formulas are data (catalog.Derivation); the math lives in
// registered Primitives. The cascade materialises results through the
// datapoints writer so model runs read fixed, traceable numbers.
package derive

import (
	"context"
	"encoding/json"
	"errors"

	"cloud.google.com/go/civil"
	"github.com/google/uuid"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
)

// Inputs maps each derivation alias to its input's history up to the date
// being computed (never later: primitives must not see the future).
type Inputs map[string][]datapoints.Resolved

// Governance are model-wide parameters primitives read.
type Governance struct {
	PercentileWindowYears     float64
	PercentileMinObservations int
	RespectDefinitionBreaks   bool
}

// Env is everything a primitive may read besides its inputs.
type Env struct {
	Series     catalog.Series // the series being computed
	Governance Governance
}

// ExplainStep is one human-readable line of a formula breakdown,
// e.g. "Earnings yield = 100 ÷ 20.78 = 4.81%".
type ExplainStep struct {
	Text   string   `json:"text"`
	Result *float64 `json:"result,omitempty"`
}

// Primitive is a reusable, pure computation. Implementations must be
// deterministic and must not read anything except their arguments.
type Primitive interface {
	Name() string
	ValidateParams(p json.RawMessage) error
	// Aliases lists required input aliases, e.g. ["x"] for percentile_rank.
	// Nil means any aliases are accepted (expr, avg).
	Aliases(p json.RawMessage) []string
	// Lookback is how much history before d the primitive needs.
	Lookback(p json.RawMessage) datapoints.Months
	Compute(ctx context.Context, env Env, d civil.Date, in Inputs, p json.RawMessage) (datapoints.Value, error)
	Explain(ctx context.Context, env Env, d civil.Date, in Inputs, p json.RawMessage) ([]ExplainStep, error)
}

// Registry holds primitives by name. It implements
// catalog.DerivationValidator.
type Registry interface {
	catalog.DerivationValidator
	Register(p Primitive)
	Get(name string) (Primitive, bool)
	Names() []string
}

// CascadeMode selects incremental or full recomputation.
type CascadeMode string

const (
	ModeIncremental CascadeMode = "incremental"
	ModeRebuild     CascadeMode = "rebuild"
)

// CascadeRequest says what changed.
type CascadeRequest struct {
	Mode              CascadeMode
	BatchID           uuid.UUID // incremental: the write batch
	SeriesKeys        []string  // rebuild: series to recompute (wildcards allowed)
	IncludeDownstream bool      // rebuild: also everything downstream
}

// CascadeResult summarises a cascade run.
type CascadeResult struct {
	Series      int
	RowsWritten int
	RowsSkipped int
}

// Progress reports cascade progress (0–100) to the job.
type Progress func(pct int, msg string)

// Cascade recomputes everything downstream of a change, layer by layer, and
// writes with origin = cascade. Re-running with the same inputs writes
// nothing (input_hash skip).
type Cascade interface {
	Run(ctx context.Context, req CascadeRequest, jobRunID int64, progress Progress) (CascadeResult, error)
}

// Explainer produces the formula breakdown for one series at one date.
type Explainer interface {
	Explain(ctx context.Context, seriesKey string, d civil.Date, o datapoints.ReadOpts) (Explanation, error)
}

// Explanation is the generated breakdown shown on the Data Points screen.
type Explanation struct {
	SeriesKey string
	ObsDate   civil.Date
	Value     datapoints.Value
	Formula   string
	Inputs    []datapoints.Resolved
	Steps     []ExplainStep
	Parts     []Explanation // for composite derivations (avg of parts)
}

var (
	ErrUnknownPrimitive = errors.New("derive: unknown primitive")
	ErrInvalidParams    = errors.New("derive: invalid params")
	ErrInsufficientData = errors.New("derive: insufficient history")
)
