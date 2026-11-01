// Package model turns score and veto datapoints into allocations. One pure
// path, Engine.Evaluate, produces every number: scheduled runs, saved
// versions, live preview, sensitivity and fragility all call it.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"cloud.google.com/go/civil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
)

// ---------------------------------------------------------------------------
// Configuration

// Normalisation modes.
const (
	NormProportional = "proportional"
	NormZeroSum      = "zero_sum"
)

// Signal is one weighted input of a scorecard.
type Signal struct {
	Key    string  `json:"key"`
	Weight float64 `json:"weight"`
}

// Scorecard scores buckets on signals and tilts them within a cap.
type Scorecard struct {
	Parent  string   `json:"parent,omitempty"` // group the buckets sit in; empty for L1
	Buckets []string `json:"buckets"`
	TiltCap float64  `json:"tilt_cap"`
	Signals []Signal `json:"signals"`
	// ScoreSeries is the series-key template, e.g. "{bucket}::{signal}".
	ScoreSeries string `json:"score_series"`
	// Vetoes maps a bucket (or "*") to trigger series-key templates.
	Vetoes map[string][]string `json:"vetoes,omitempty"`
}

// SectorSatellite is the sector sleeve carved out of equity.
type SectorSatellite struct {
	Parent           string   `json:"parent"`
	Sectors          []string `json:"sectors"`
	Signals          []Signal `json:"signals"`
	ScoreSeries      string   `json:"score_series"`
	SleeveCap        float64  `json:"sleeve_cap"`
	MaxSectors       int      `json:"max_sectors"`
	QualifyThreshold float64  `json:"qualify_threshold"`
	MaxQuartersHeld  int      `json:"max_quarters_held"`
}

// ConstraintRef applies a registered constraint with params.
type ConstraintRef struct {
	Name      string          `json:"name"`
	Scorecard string          `json:"scorecard,omitempty"`
	Bucket    string          `json:"bucket,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
}

// Governance holds model-wide parameters that primitives also read.
type Governance struct {
	PercentileWindowYears     float64 `json:"percentile_window_years"`
	PercentileMinObservations int     `json:"percentile_min_observations"`
	RespectDefinitionBreaks   bool    `json:"respect_definition_breaks"`
}

// Config is one versioned model definition (the workbook's Weights & Rules).
type Config struct {
	Steps           []string                `json:"steps"`
	MissingScore    float64                 `json:"missing_score"`
	Normalisation   string                  `json:"normalisation"`
	Scorecards      map[string]Scorecard    `json:"scorecards"`
	SectorSatellite *SectorSatellite        `json:"sector_satellite,omitempty"`
	Constraints     []ConstraintRef         `json:"constraints,omitempty"`
	ReviewCalendar  map[string][]civil.Date `json:"review_calendar,omitempty"`
	Governance      Governance              `json:"governance"`
}

// ConfigStatus is a config version's lifecycle state.
type ConfigStatus string

const (
	ConfigDraft   ConfigStatus = "draft"
	ConfigActive  ConfigStatus = "active"
	ConfigRetired ConfigStatus = "retired"
)

// ConfigVersion is a stored config.
type ConfigVersion struct {
	ID            int64
	ModelKey      string
	Version       int
	Status        ConfigStatus
	Config        Config
	Note          string
	EffectiveFrom *civil.Date
	CreatedBy     *int64
	CreatedAt     time.Time
}

// PatchOp is one RFC 6902 operation on a config.
type PatchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value,omitempty"`
}

// Configs stores versioned configs. Create validates; Activate retires the
// previously active version in the same transaction.
type Configs interface {
	Get(ctx context.Context, model string, version int) (ConfigVersion, error)
	Active(ctx context.Context, model string) (ConfigVersion, error)
	List(ctx context.Context, model string) ([]ConfigVersion, error)
	Create(ctx context.Context, tx pgx.Tx, model string, base int, patch []PatchOp, note string, actorID int64) (ConfigVersion, error)
	Activate(ctx context.Context, tx pgx.Tx, model string, version int) error
	Diff(ctx context.Context, model string, from, to int) ([]PatchOp, error)
}

// Validator checks a config: weights sum to 1, referenced steps and
// constraints are registered, referenced series exist. Errors carry JSON
// pointers (apperr.Detail.Path).
type Validator interface {
	Validate(ctx context.Context, c Config) error
}

// ---------------------------------------------------------------------------
// Risk profiles and portfolios

// RiskProfile holds neutral weights per node for one investor type.
type RiskProfile struct {
	ID             int64
	Key            string
	Name           string
	Version        int
	NeutralWeights map[string]decimal.Decimal
}

// RiskProfiles stores profiles. Save creates a new version.
type RiskProfiles interface {
	Get(ctx context.Context, key string) (RiskProfile, error) // latest version
	List(ctx context.Context) ([]RiskProfile, error)
	Save(ctx context.Context, tx pgx.Tx, p RiskProfile) (RiskProfile, error)
}

// Portfolio binds a model to a risk profile. One ('core') for now.
type Portfolio struct {
	ID            int64
	Key           string
	Name          string
	ModelKey      string
	RiskProfileID int64
}

// Portfolios reads portfolios.
type Portfolios interface {
	Get(ctx context.Context, key string) (Portfolio, error)
}

// ---------------------------------------------------------------------------
// Engine

// Input is one score, trigger or veto value fed to the engine.
type Input struct {
	Value       datapoints.Value `json:"value"`
	DatapointID int64            `json:"datapoint_id,omitempty"`
	Source      string           `json:"source"`
	ObservedAt  *civil.Date      `json:"observed_at,omitempty"`
}

// Params are human decisions fed into a run.
type Params struct {
	SectorsHeld []string `json:"sectors_held,omitempty"`
}

// NodeOverride pins a run node (e.g. force a veto) for a period.
type NodeOverride struct {
	ID         uuid.UUID
	NodeKey    string
	AsOf       civil.Date
	Value      datapoints.Value
	Reason     string
	ActorID    int64
	ValidUntil *civil.Date
}

// EvalInput is everything Evaluate needs. It is a value: Evaluate never reads
// the database, so the same input always yields the same result.
type EvalInput struct {
	AsOf      civil.Date
	Cutoff    time.Time
	Config    Config
	Profile   RiskProfile
	Inputs    map[string]Input // series key → value
	Params    Params
	Overrides []NodeOverride
	History   SatelliteHistory
}

// SatelliteHistory is what tenure rules need from earlier published versions.
type SatelliteHistory struct {
	QuartersHeld map[string]int // sector → consecutive quarters held
	ExtremeRuns  map[string]int // bucket → consecutive reviews with an extreme composite
}

// NodeInputs records how a node was computed (lineage).
type NodeInputs struct {
	Datapoints []int64            `json:"datapoints,omitempty"`
	Nodes      []string           `json:"nodes,omitempty"`
	Params     map[string]float64 `json:"params,omitempty"`
}

// Node is one computed value: composite, veto, tilt, weight, rollup.
type Node struct {
	Key        string
	Num        *float64
	Bool       *bool
	Step       string
	Formula    string
	Inputs     NodeInputs
	IsOverride bool
}

// Warning is a non-fatal issue, e.g. a missing score that used the default.
type Warning struct {
	Code   string  `json:"code"`
	Series string  `json:"series,omitempty"`
	Node   string  `json:"node,omitempty"`
	Used   float64 `json:"used,omitempty"`
}

// AllocationLine is one line of the final allocation. Weights are exact
// decimals; each group and the portfolio sum to exactly 1.
type AllocationLine struct {
	LineItem        string
	Group           string
	GroupWeight     decimal.Decimal
	WithinGroup     decimal.Decimal
	PortfolioWeight decimal.Decimal
	NeutralWeight   decimal.Decimal
	Veto            bool
	Sleeve          bool
}

// Result is the output of Evaluate.
type Result struct {
	Nodes      map[string]Node
	Allocation []AllocationLine
	Warnings   []Warning
}

// State is threaded through the steps of one evaluation.
type State struct {
	In    EvalInput
	Nodes map[string]Node
	Warn  []Warning
	Lines []AllocationLine
}

// Step is one stage of the pipeline (composite, veto, tilt, …). Steps are
// pure functions of State.
type Step interface {
	Name() string
	Run(ctx context.Context, s *State) error
}

// Constraint adjusts tilts or weights for one scorecard.
type Constraint interface {
	Name() string
	ValidateParams(p json.RawMessage) error
	Apply(ctx context.Context, s *State, ref ConstraintRef) error
}

// Engine evaluates a model. Implementations hold the step and constraint
// registries; Evaluate must not perform I/O (target p95 < 50 ms).
type Engine interface {
	Evaluate(ctx context.Context, in EvalInput) (Result, error)
	RegisterStep(s Step)
	RegisterConstraint(c Constraint)
}

// InputLoader assembles an EvalInput from stored datapoints at a cutoff.
type InputLoader interface {
	Load(ctx context.Context, model string, asOf civil.Date, cutoff time.Time, c Config, p Params) (EvalInput, error)
}

// ---------------------------------------------------------------------------
// Runs

// RunStatus is a model run's lifecycle state.
type RunStatus string

const (
	RunRunning    RunStatus = "running"
	RunDraft      RunStatus = "draft"
	RunApproved   RunStatus = "approved"
	RunPublished  RunStatus = "published"
	RunSuperseded RunStatus = "superseded"
	RunFailed     RunStatus = "failed"
)

// Run is a stored model run.
type Run struct {
	ID              int64
	UUID            uuid.UUID
	ModelKey        string
	ConfigVersion   int
	RiskProfileKey  string
	AsOf            civil.Date
	KnowledgeCutoff time.Time
	Params          Params
	Status          RunStatus
	Warnings        []Warning
	JobRunID        *int64
	CreatedBy       *int64
	ApprovedBy      *int64
	CreatedAt       time.Time
	ApprovedAt      *time.Time
}

// RunFilter narrows Runs.List.
type RunFilter struct {
	Model  string
	Status RunStatus
	From   *civil.Date
	Page   paging.Request
}

// Runs stores runs, their node values and allocations. Values and
// allocations are append-only.
type Runs interface {
	Create(ctx context.Context, tx pgx.Tx, r Run, res Result) (Run, error)
	Get(ctx context.Context, id uuid.UUID) (Run, error)
	List(ctx context.Context, f RunFilter) (paging.Page[Run], error)
	Values(ctx context.Context, id uuid.UUID, step string) ([]Node, error)
	Allocation(ctx context.Context, id uuid.UUID) ([]AllocationLine, error)
	Approve(ctx context.Context, tx pgx.Tx, id uuid.UUID, actorID int64, note string) error
	Publish(ctx context.Context, tx pgx.Tx, id uuid.UUID, actorID int64) error
}

// Overrides stores node overrides (soft-deleted).
type Overrides interface {
	Active(ctx context.Context, model string, asOf civil.Date) ([]NodeOverride, error)
	Create(ctx context.Context, tx pgx.Tx, model string, o NodeOverride) (NodeOverride, error)
	Delete(ctx context.Context, tx pgx.Tx, id uuid.UUID, actorID int64) error
}

// ---------------------------------------------------------------------------
// Draft and versions (CORE dashboard)

// Edit is one change to the shared draft.
type Edit struct {
	Op         string            `json:"op"` // set_param | set_score | set_rubric | clear_override
	Path       string            `json:"path,omitempty"`
	Key        string            `json:"key,omitempty"`
	Value      json.RawMessage   `json:"value,omitempty"`
	Conditions map[string]string `json:"conditions,omitempty"`
	Expires    *civil.Date       `json:"expires,omitempty"`
}

// Draft is a model's shared working state.
type Draft struct {
	ModelKey      string
	Rev           int
	Config        Config
	BaseVersionID *uuid.UUID
	UpdatedBy     *int64
	UpdatedAt     time.Time
}

// Preview is the result of evaluating the draft with unsaved edits.
type Preview struct {
	Result     Result
	DiffVsSave []AllocationMove
}

// AllocationMove is one line's change between two states.
type AllocationMove struct {
	Node  string
	From  decimal.Decimal
	To    decimal.Decimal
	Delta decimal.Decimal
}

// Drafts manages the shared draft with optimistic concurrency: a stale rev
// returns ErrStaleRev.
type Drafts interface {
	Get(ctx context.Context, model string) (Draft, error)
	// Evaluate previews edits without writing anything.
	Evaluate(ctx context.Context, model string, rev int, edits []Edit) (Preview, error)
	// Apply persists edits: scores and rubric answers become datapoints with
	// source override/analyst; params update the draft config.
	Apply(ctx context.Context, tx pgx.Tx, model string, rev int, edits []Edit, reason string, actorID int64) (Draft, error)
	Reset(ctx context.Context, tx pgx.Tx, model string, actorID int64) (Draft, error)
}

// VersionKind distinguishes saved reviews from publications.
type VersionKind string

const (
	VersionReview    VersionKind = "review"
	VersionPublished VersionKind = "published"
)

// RevertMode chooses what a revert restores.
type RevertMode string

const (
	RevertManualAndParams RevertMode = "manual_and_params"
	RevertAll             RevertMode = "all"
)

// Version is an immutable snapshot of the draft.
type Version struct {
	ID          uuid.UUID
	ModelKey    string
	Portfolio   string
	Kind        VersionKind
	Status      string // active | superseded | saved | deleted
	Label       string
	Reason      string
	Config      Config
	Inputs      map[string]Input
	RunID       uuid.UUID
	AsOf        civil.Date
	CreatedBy   *int64
	CreatedAt   time.Time
	PublishedAt *time.Time
}

// Comparison is the difference between two of: draft, a version, neutral.
type Comparison struct {
	Moves  []AllocationMove
	Inputs []InputChange
}

// InputChange is one changed score, veto or parameter.
type InputChange struct {
	Key    string
	Kind   string // score | veto | parameter
	Source string
	From   json.RawMessage
	To     json.RawMessage
}

// Versions freezes, publishes, compares and reverts.
type Versions interface {
	Save(ctx context.Context, tx pgx.Tx, model string, kind VersionKind, label, reason string, actorID int64) (Version, error)
	Publish(ctx context.Context, tx pgx.Tx, id uuid.UUID, actorID int64) (Version, error)
	Get(ctx context.Context, id uuid.UUID) (Version, error)
	Current(ctx context.Context, portfolio string) (Version, error) // active published
	List(ctx context.Context, portfolio string, p paging.Request) (paging.Page[Version], error)
	Compare(ctx context.Context, a, b string) (Comparison, error) // "draft" | "neutral" | "published" | version uuid
	Revert(ctx context.Context, tx pgx.Tx, id uuid.UUID, mode RevertMode, actorID int64) (Draft, Comparison, error)
	Delete(ctx context.Context, tx pgx.Tx, id uuid.UUID, actorID int64) error
}

// ---------------------------------------------------------------------------
// Signal analytics

// Contribution is (score − 50) × weight, in composite points.
type Contribution struct {
	Node   string
	Signal string
	Score  float64
	Weight float64
	Points float64
}

// Sensitivity is the max line move when one score moves ±bump.
type Sensitivity struct {
	Key      string
	ImpactPP float64
}

// Fragility is how far a rollup moves when signal weights move ±perturbation.
type Fragility struct {
	Node    string
	Min     float64
	Max     float64
	RangePP float64
}

// Analytics derives signal views from the draft through Engine.Evaluate.
type Analytics interface {
	Contributions(ctx context.Context, model string) ([]Contribution, error)
	Attribution(ctx context.Context, model string) ([]AllocationMove, error)
	Sensitivity(ctx context.Context, model string, bump float64, limit int) ([]Sensitivity, error)
	Fragility(ctx context.Context, model string, perturbation float64) ([]Fragility, error)
}

var (
	ErrNotFound     = errors.New("model: not found")
	ErrStaleRev     = errors.New("model: draft revision is stale")
	ErrInvalidState = errors.New("model: invalid state transition")
	// ErrActivePublished is returned when deleting the active published version.
	ErrActivePublished = errors.New("model: cannot delete the active published version")
)
