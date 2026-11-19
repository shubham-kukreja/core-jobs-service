package model

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"time"

	"cloud.google.com/go/civil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/model/internal/db"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
)

// Store implements Configs, RiskProfiles, Portfolios, Runs and Overrides on
// Postgres.
type Store struct {
	pool      *pgxpool.Pool
	q         *db.Queries
	validator Validator
}

var _ Configs = (*Store)(nil)

// NewStore returns the model store. v validates configs on create.
func NewStore(pool *pgxpool.Pool, v Validator) *Store {
	return &Store{pool: pool, q: db.New(pool), validator: v}
}

func notFound(what string) error {
	return apperr.Wrap(ErrNotFound, apperr.NotFound, what+" not found")
}

func invalidState(msg string) error {
	return apperr.Wrap(ErrInvalidState, apperr.Conflict, msg)
}

func noRows(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound(what)
	}
	return err
}

// ---- conversions ---------------------------------------------------------------

func pgDate(d civil.Date) pgtype.Date {
	return pgtype.Date{Time: d.In(time.UTC), Valid: true}
}

func pgDatePtr(d *civil.Date) pgtype.Date {
	if d == nil {
		return pgtype.Date{}
	}
	return pgDate(*d)
}

func fromPGDate(d pgtype.Date) civil.Date { return civil.DateOf(d.Time) }

func fromPGDatePtr(d pgtype.Date) *civil.Date {
	if !d.Valid {
		return nil
	}
	c := fromPGDate(d)
	return &c
}

func pgNumeric(d decimal.Decimal) pgtype.Numeric {
	return pgtype.Numeric{Int: new(big.Int).Set(d.Coefficient()), Exp: d.Exponent(), Valid: true}
}

func fromPGNumeric(n pgtype.Numeric) decimal.Decimal {
	if !n.Valid || n.Int == nil {
		return decimal.Zero
	}
	return decimal.NewFromBigInt(n.Int, n.Exp)
}

func strPtr(s string) *string {
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

// ---- configs -------------------------------------------------------------------

func toConfigVersion(id int64, model string, version int32, status string, raw []byte, note *string, eff pgtype.Date, by *int64, at time.Time) (ConfigVersion, error) {
	cv := ConfigVersion{ID: id, ModelKey: model, Version: int(version), Status: ConfigStatus(status), Note: deref(note),
		EffectiveFrom: fromPGDatePtr(eff), CreatedBy: by, CreatedAt: at}
	if err := json.Unmarshal(raw, &cv.Config); err != nil {
		return cv, fmt.Errorf("model: config %s v%d: %w", model, version, err)
	}
	return cv, nil
}

func (s *Store) Get(ctx context.Context, model string, version int) (ConfigVersion, error) {
	r, err := s.q.GetConfig(ctx, db.GetConfigParams{ModelKey: model, Version: int32(version)}) //nolint:gosec // versions are small
	if err != nil {
		return ConfigVersion{}, noRows(err, fmt.Sprintf("config %s v%d", model, version))
	}
	return toConfigVersion(r.ID, r.ModelKey, r.Version, r.Status, r.Config, r.Note, r.EffectiveFrom, r.CreatedBy, r.CreatedAt)
}

func (s *Store) Active(ctx context.Context, model string) (ConfigVersion, error) {
	r, err := s.q.ActiveConfig(ctx, model)
	if err != nil {
		return ConfigVersion{}, noRows(err, "active config for "+model)
	}
	return toConfigVersion(r.ID, r.ModelKey, r.Version, r.Status, r.Config, r.Note, r.EffectiveFrom, r.CreatedBy, r.CreatedAt)
}

func (s *Store) List(ctx context.Context, model string) ([]ConfigVersion, error) {
	rows, err := s.q.ListConfigs(ctx, model)
	if err != nil {
		return nil, err
	}
	out := make([]ConfigVersion, 0, len(rows))
	for _, r := range rows {
		cv, err := toConfigVersion(r.ID, r.ModelKey, r.Version, r.Status, r.Config, r.Note, r.EffectiveFrom, r.CreatedBy, r.CreatedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, cv)
	}
	return out, nil
}

// Create applies patch to version base (0 = the active version), validates
// the result and stores it as the next version, status draft.
func (s *Store) Create(ctx context.Context, tx pgx.Tx, model string, base int, patch []PatchOp, note string, actorID int64) (ConfigVersion, error) {
	var from ConfigVersion
	var err error
	if base == 0 {
		from, err = s.Active(ctx, model)
	} else {
		from, err = s.Get(ctx, model, base)
	}
	if err != nil {
		return ConfigVersion{}, err
	}
	next, err := ApplyPatch(from.Config, patch)
	if err != nil {
		return ConfigVersion{}, err
	}
	return s.insertConfig(ctx, tx, model, 0, next, ConfigDraft, note, actorID)
}

func (s *Store) insertConfig(ctx context.Context, tx pgx.Tx, model string, version int, c Config, status ConfigStatus, note string, actorID int64) (ConfigVersion, error) {
	if err := s.validator.Validate(ctx, c); err != nil {
		return ConfigVersion{}, err
	}
	q := s.q.WithTx(tx)
	mid, err := q.GetModelID(ctx, model)
	if err != nil {
		return ConfigVersion{}, noRows(err, "model "+model)
	}
	if version == 0 {
		maxV, err := q.MaxConfigVersion(ctx, mid)
		if err != nil {
			return ConfigVersion{}, err
		}
		version = int(maxV) + 1
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return ConfigVersion{}, err
	}
	var by *int64
	if actorID != 0 {
		by = &actorID
	}
	r, err := q.InsertConfig(ctx, db.InsertConfigParams{ModelID: mid, Version: int32(version), Status: string(status), //nolint:gosec // small
		Config: raw, Note: strPtr(note), CreatedBy: by})
	if err != nil {
		return ConfigVersion{}, err
	}
	return ConfigVersion{ID: r.ID, ModelKey: model, Version: version, Status: status, Config: c, Note: note, CreatedBy: by, CreatedAt: r.CreatedAt}, nil
}

// Activate makes version the active config, retiring the current one.
func (s *Store) Activate(ctx context.Context, tx pgx.Tx, model string, version int) error {
	q := s.q.WithTx(tx)
	mid, err := q.GetModelID(ctx, model)
	if err != nil {
		return noRows(err, "model "+model)
	}
	cur, err := s.Active(ctx, model)
	if err == nil && cur.Version == version {
		return nil
	}
	if err := q.RetireActiveConfig(ctx, mid); err != nil {
		return err
	}
	n, err := q.ActivateConfig(ctx, db.ActivateConfigParams{ModelID: mid, Version: int32(version)}) //nolint:gosec // small
	if err != nil {
		return err
	}
	if n == 0 {
		return notFound(fmt.Sprintf("config %s v%d", model, version))
	}
	return nil
}

// Diff returns the patch that turns version from into version to.
func (s *Store) Diff(ctx context.Context, model string, from, to int) ([]PatchOp, error) {
	a, err := s.Get(ctx, model, from)
	if err != nil {
		return nil, err
	}
	b, err := s.Get(ctx, model, to)
	if err != nil {
		return nil, err
	}
	return DiffConfigs(a.Config, b.Config)
}

// SeedConfig makes sure model exists and holds config c as version, active.
// Versions are immutable: re-seeding an existing version with a different
// config is an error (bump the version instead). It reports whether it
// wrote anything.
func (s *Store) SeedConfig(ctx context.Context, tx pgx.Tx, model, name string, version int, c Config, note string, actorID int64) (bool, error) {
	q := s.q.WithTx(tx)
	if _, err := q.UpsertModel(ctx, db.UpsertModelParams{Key: model, Name: name}); err != nil {
		return false, err
	}
	existing, err := q.GetConfig(ctx, db.GetConfigParams{ModelKey: model, Version: int32(version)}) //nolint:gosec // small
	switch {
	case err == nil:
		var have Config
		if err := json.Unmarshal(existing.Config, &have); err != nil {
			return false, err
		}
		if !reflect.DeepEqual(normalizeConfig(have), normalizeConfig(c)) {
			return false, apperr.New(apperr.Conflict, fmt.Sprintf("model %s config v%d already exists with different contents; bump the version", model, version))
		}
		if existing.Status == string(ConfigActive) {
			return false, nil
		}
		return true, s.Activate(ctx, tx, model, version)
	case !errors.Is(err, pgx.ErrNoRows):
		return false, err
	}
	if active, err := q.ActiveConfig(ctx, model); err == nil && int(active.Version) > version {
		return false, apperr.New(apperr.Conflict, fmt.Sprintf("model %s already runs v%d, newer than seed v%d", model, active.Version, version))
	}
	if _, err := s.insertConfig(ctx, tx, model, version, c, ConfigDraft, note, actorID); err != nil {
		return false, err
	}
	return true, s.Activate(ctx, tx, model, version)
}

// normalizeConfig round-trips through JSON so nil and empty compare equal.
func normalizeConfig(c Config) any {
	b, _ := json.Marshal(c)
	var v any
	_ = json.Unmarshal(b, &v)
	return v
}

// ---- risk profiles ---------------------------------------------------------------

func toProfile(id int64, key, name string, version int32, raw []byte) (RiskProfile, error) {
	p := RiskProfile{ID: id, Key: key, Name: name, Version: int(version)}
	if err := json.Unmarshal(raw, &p.NeutralWeights); err != nil {
		return p, fmt.Errorf("model: risk profile %s: %w", key, err)
	}
	return p, nil
}

// GetProfile returns a risk profile's latest version.
func (s *Store) GetProfile(ctx context.Context, key string) (RiskProfile, error) {
	r, err := s.q.LatestProfile(ctx, key)
	if err != nil {
		return RiskProfile{}, noRows(err, "risk profile "+key)
	}
	return toProfile(r.ID, r.Key, r.Name, r.Version, r.NeutralWeights)
}

// ProfileByID returns one stored profile version.
func (s *Store) ProfileByID(ctx context.Context, id int64) (RiskProfile, error) {
	r, err := s.q.ProfileByID(ctx, id)
	if err != nil {
		return RiskProfile{}, noRows(err, fmt.Sprintf("risk profile %d", id))
	}
	return toProfile(r.ID, r.Key, r.Name, r.Version, r.NeutralWeights)
}

// ListProfiles returns every profile's latest version.
func (s *Store) ListProfiles(ctx context.Context) ([]RiskProfile, error) {
	rows, err := s.q.ListLatestProfiles(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RiskProfile, 0, len(rows))
	for _, r := range rows {
		p, err := toProfile(r.ID, r.Key, r.Name, r.Version, r.NeutralWeights)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// SaveProfile stores p as a new version unless it equals the latest.
func (s *Store) SaveProfile(ctx context.Context, tx pgx.Tx, p RiskProfile) (RiskProfile, error) {
	if err := validateProfile(p); err != nil {
		return RiskProfile{}, err
	}
	q := s.q.WithTx(tx)
	next := 1
	if r, err := q.LatestProfile(ctx, p.Key); err == nil {
		cur, err := toProfile(r.ID, r.Key, r.Name, r.Version, r.NeutralWeights)
		if err != nil {
			return RiskProfile{}, err
		}
		if cur.Name == p.Name && sameWeights(cur.NeutralWeights, p.NeutralWeights) {
			return cur, nil
		}
		next = cur.Version + 1
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return RiskProfile{}, err
	}
	raw, err := json.Marshal(p.NeutralWeights)
	if err != nil {
		return RiskProfile{}, err
	}
	id, err := q.InsertProfile(ctx, db.InsertProfileParams{Key: p.Key, Name: p.Name, Version: int32(next), NeutralWeights: raw})
	if err != nil {
		return RiskProfile{}, err
	}
	p.ID, p.Version = id, next
	return p, nil
}

func sameWeights(a, b map[string]decimal.Decimal) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || !w.Equal(v) {
			return false
		}
	}
	return true
}

func validateProfile(p RiskProfile) error {
	var det []apperr.Detail
	if p.Key == "" || p.Name == "" {
		det = append(det, apperr.Detail{Field: "key", Reason: "required"})
	}
	if len(p.NeutralWeights) == 0 {
		det = append(det, apperr.Detail{Field: "neutral_weights", Reason: "required"})
	}
	for k, w := range p.NeutralWeights {
		if w.Sign() < 0 || w.GreaterThan(decimal.NewFromInt(1)) || w.Exponent() < -weightPlaces {
			det = append(det, apperr.Detail{Path: "/neutral_weights/" + k, Reason: "out_of_range",
				Meta: map[string]any{"min": 0, "max": 1, "max_decimals": weightPlaces}})
		}
	}
	if len(det) > 0 {
		return apperr.New(apperr.ValidationFailed, "Invalid risk profile", det...)
	}
	return nil
}

// ---- portfolios --------------------------------------------------------------------

// Portfolio returns a portfolio by key.
func (s *Store) Portfolio(ctx context.Context, key string) (Portfolio, error) {
	r, err := s.q.GetPortfolio(ctx, key)
	if err != nil {
		return Portfolio{}, noRows(err, "portfolio "+key)
	}
	return Portfolio{ID: r.ID, Key: r.Key, Name: r.Name, ModelKey: r.ModelKey, RiskProfileID: r.RiskProfileID}, nil
}

// SeedPortfolio creates or repoints a portfolio at the profile's latest version.
func (s *Store) SeedPortfolio(ctx context.Context, tx pgx.Tx, key, name, model, profile string) error {
	q := s.q.WithTx(tx)
	mid, err := q.GetModelID(ctx, model)
	if err != nil {
		return noRows(err, "model "+model)
	}
	p, err := q.LatestProfile(ctx, profile)
	if err != nil {
		return noRows(err, "risk profile "+profile)
	}
	_, err = q.UpsertPortfolio(ctx, db.UpsertPortfolioParams{Key: key, Name: name, ModelID: mid, RiskProfileID: p.ID})
	return err
}

// ---- runs --------------------------------------------------------------------------

// RunRecord is everything a run needs beyond its result to be reproduced:
// stored in model_runs.params next to the human params.
type RunRecord struct {
	Params
	History   SatelliteHistory `json:"history"`
	Overrides []NodeOverride   `json:"overrides,omitempty"`
}

// inputPrefix marks run_values rows holding the run's inputs.
const inputPrefix = "input:"

// storedInputs is how a node's lineage and formula are kept in run_values.inputs.
type storedInputs struct {
	NodeInputs
	Formula string `json:"formula,omitempty"`
}

// InputHash is the run's derivation hash: SHA-256 of everything Evaluate
// reads, so two runs with the same hash have the same result. Empty and
// nil collections hash the same, so a replayed input matches.
func InputHash(in EvalInput) ([]byte, error) {
	if len(in.Overrides) == 0 {
		in.Overrides = nil
	}
	if len(in.Params.SectorsHeld) == 0 {
		in.Params.SectorsHeld = nil
	}
	if len(in.History.QuartersHeld) == 0 {
		in.History.QuartersHeld = nil
	}
	if len(in.History.ExtremeRuns) == 0 {
		in.History.ExtremeRuns = nil
	}
	b, err := json.Marshal(struct {
		AsOf      civil.Date                 `json:"as_of"`
		Config    Config                     `json:"config"`
		Profile   map[string]decimal.Decimal `json:"profile"`
		Inputs    map[string]Input           `json:"inputs"`
		Params    Params                     `json:"params"`
		Overrides []NodeOverride             `json:"overrides"`
		History   SatelliteHistory           `json:"history"`
	}{in.AsOf, in.Config, in.Profile.NeutralWeights, in.Inputs, in.Params, in.Overrides, in.History})
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(b)
	return h[:], nil
}

// CreateRun stores a run, its inputs, node values and allocation in tx.
// r.Status must be draft or published; publishing supersedes the previous
// published run for the same model and risk profile.
func (s *Store) CreateRun(ctx context.Context, tx pgx.Tx, r Run, in EvalInput, res Result) (Run, error) {
	if r.Status != RunDraft && r.Status != RunPublished {
		return Run{}, fmt.Errorf("model: new run status %q", r.Status)
	}
	q := s.q.WithTx(tx)
	mid, err := q.GetModelID(ctx, r.ModelKey)
	if err != nil {
		return Run{}, noRows(err, "model "+r.ModelKey)
	}
	cfg, err := q.GetConfig(ctx, db.GetConfigParams{ModelKey: r.ModelKey, Version: int32(r.ConfigVersion)}) //nolint:gosec // small
	if err != nil {
		return Run{}, noRows(err, fmt.Sprintf("config %s v%d", r.ModelKey, r.ConfigVersion))
	}
	hash, err := InputHash(in)
	if err != nil {
		return Run{}, err
	}
	params, err := json.Marshal(RunRecord{Params: in.Params, History: in.History, Overrides: in.Overrides})
	if err != nil {
		return Run{}, err
	}
	warnings := res.Warnings
	if warnings == nil {
		warnings = []Warning{}
	}
	warn, err := json.Marshal(warnings)
	if err != nil {
		return Run{}, err
	}
	row, err := q.InsertRun(ctx, db.InsertRunParams{
		ModelID: mid, ConfigID: cfg.ID, RiskProfileID: in.Profile.ID, AsOf: pgDate(in.AsOf), KnowledgeCutoff: in.Cutoff,
		Params: params, DerivationHash: hash, Status: string(r.Status), Warnings: warn, JobRunID: r.JobRunID, CreatedBy: r.CreatedBy,
	})
	if err != nil {
		return Run{}, err
	}
	values := make([]db.InsertRunValuesParams, 0, len(res.Nodes)+len(in.Inputs))
	for key, inp := range in.Inputs {
		si := storedInputs{Formula: inp.Source}
		if inp.DatapointID != 0 {
			si.Datapoints = []int64{inp.DatapointID}
		}
		if inp.ObservedAt != nil {
			si.Formula += " @" + inp.ObservedAt.String()
		}
		raw, err := json.Marshal(si)
		if err != nil {
			return Run{}, err
		}
		values = append(values, db.InsertRunValuesParams{RunID: row.ID, NodeKey: inputPrefix + key,
			ValueNum: inp.Value.Num, ValueBool: inp.Value.Bool, Step: "input", Inputs: raw})
	}
	for _, n := range res.Nodes {
		raw, err := json.Marshal(storedInputs{NodeInputs: n.Inputs, Formula: n.Formula})
		if err != nil {
			return Run{}, err
		}
		values = append(values, db.InsertRunValuesParams{RunID: row.ID, NodeKey: n.Key, ValueNum: n.Num, ValueBool: n.Bool,
			Step: n.Step, Inputs: raw, IsOverride: n.IsOverride})
	}
	if _, err := q.InsertRunValues(ctx, values); err != nil {
		return Run{}, err
	}
	allocs := make([]db.InsertAllocationsParams, 0, len(res.Allocation))
	for _, l := range res.Allocation {
		allocs = append(allocs, db.InsertAllocationsParams{RunID: row.ID, RiskProfileID: in.Profile.ID, LineItem: l.LineItem,
			GroupKey: l.Group, GroupWeight: pgNumeric(l.GroupWeight), WithinGroup: pgNumeric(l.WithinGroup),
			PortfolioWeight: pgNumeric(l.PortfolioWeight), NeutralWeight: pgNumeric(l.NeutralWeight)})
	}
	if _, err := q.InsertAllocations(ctx, allocs); err != nil {
		return Run{}, err
	}
	if r.Status == RunPublished {
		if err := q.SupersedePublished(ctx, db.SupersedePublishedParams{ModelID: mid, RiskProfileID: in.Profile.ID, RunID: row.ID}); err != nil {
			return Run{}, err
		}
	}
	r.ID, r.UUID, r.CreatedAt, r.DerivationHash = row.ID, row.Uuid, row.CreatedAt, hash
	r.RiskProfileKey, r.AsOf, r.KnowledgeCutoff, r.Params, r.Warnings = in.Profile.Key, in.AsOf, in.Cutoff, in.Params, warnings
	return r, nil
}

func toRun(r db.GetRunRow) (Run, error) {
	run := Run{ID: r.ID, UUID: r.Uuid, ModelKey: r.ModelKey, ConfigVersion: int(r.ConfigVersion), RiskProfileKey: r.RiskProfileKey,
		AsOf: fromPGDate(r.AsOf), KnowledgeCutoff: r.KnowledgeCutoff, Status: RunStatus(r.Status), JobRunID: r.JobRunID,
		CreatedBy: r.CreatedBy, ApprovedBy: r.ApprovedBy, CreatedAt: r.CreatedAt, ApprovedAt: r.ApprovedAt, DerivationHash: r.DerivationHash}
	var rec RunRecord
	if err := json.Unmarshal(r.Params, &rec); err != nil {
		return run, err
	}
	run.Params = rec.Params
	if err := json.Unmarshal(r.Warnings, &run.Warnings); err != nil {
		return run, err
	}
	return run, nil
}

// GetRun returns a run by UUID.
func (s *Store) GetRun(ctx context.Context, id uuid.UUID) (Run, error) {
	r, err := s.q.GetRun(ctx, id)
	if err != nil {
		return Run{}, noRows(err, "run "+id.String())
	}
	return toRun(r)
}

// ListRuns pages runs newest first; the cursor is the last run's id.
func (s *Store) ListRuns(ctx context.Context, f RunFilter) (paging.Page[Run], error) {
	req := f.Page.Normalize()
	p := db.ListRunsParams{ModelKey: strPtr(f.Model), Lim: int32(req.Limit + 1)} //nolint:gosec // bounded
	if f.Status != "" {
		st := string(f.Status)
		p.Status = &st
	}
	if f.From != nil {
		p.FromDate = pgDate(*f.From)
	}
	if req.After != "" {
		var id int64
		if _, err := fmt.Sscan(req.After, &id); err != nil {
			return paging.Page[Run]{}, apperr.New(apperr.ValidationFailed, "Invalid cursor")
		}
		p.BeforeID = &id
	}
	rows, err := s.q.ListRuns(ctx, p)
	if err != nil {
		return paging.Page[Run]{}, err
	}
	var page paging.Page[Run]
	for i, r := range rows {
		if i == req.Limit {
			page.Next = fmt.Sprint(page.Items[i-1].ID)
			break
		}
		run, err := toRun(db.GetRunRow(r))
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, run)
	}
	return page, nil
}

// RunValues returns a run's nodes, optionally one step's ("input" for the
// run's inputs, keyed by series).
func (s *Store) RunValues(ctx context.Context, id uuid.UUID, step string) ([]Node, error) {
	r, err := s.q.GetRun(ctx, id)
	if err != nil {
		return nil, noRows(err, "run "+id.String())
	}
	rows, err := s.q.RunValues(ctx, db.RunValuesParams{RunID: r.ID, Step: strPtr(step)})
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(rows))
	for _, v := range rows {
		var si storedInputs
		if err := json.Unmarshal(v.Inputs, &si); err != nil {
			return nil, err
		}
		out = append(out, Node{Key: v.NodeKey, Num: v.ValueNum, Bool: v.ValueBool, Step: v.Step, Formula: si.Formula,
			Inputs: si.NodeInputs, IsOverride: v.IsOverride})
	}
	return out, nil
}

// Allocation returns a run's allocation lines.
func (s *Store) Allocation(ctx context.Context, id uuid.UUID) ([]AllocationLine, error) {
	r, err := s.q.GetRun(ctx, id)
	if err != nil {
		return nil, noRows(err, "run "+id.String())
	}
	rows, err := s.q.RunAllocation(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	out := make([]AllocationLine, 0, len(rows))
	for _, a := range rows {
		out = append(out, AllocationLine{LineItem: a.LineItem, Group: a.GroupKey, GroupWeight: fromPGNumeric(a.GroupWeight),
			WithinGroup: fromPGNumeric(a.WithinGroup), PortfolioWeight: fromPGNumeric(a.PortfolioWeight), NeutralWeight: fromPGNumeric(a.NeutralWeight)})
	}
	return out, nil
}

// Approve moves a draft run to approved.
func (s *Store) Approve(ctx context.Context, tx pgx.Tx, id uuid.UUID, actorID int64, _ string) error {
	q := s.q.WithTx(tx)
	r, err := q.LockRun(ctx, id)
	if err != nil {
		return noRows(err, "run "+id.String())
	}
	if r.Status != string(RunDraft) {
		return invalidState(fmt.Sprintf("run is %s; only a draft can be approved", r.Status))
	}
	return q.ApproveRun(ctx, db.ApproveRunParams{ID: r.ID, ActorID: &actorID})
}

// Publish makes a draft or approved run the published allocation for its
// model and risk profile, superseding the previous one.
func (s *Store) Publish(ctx context.Context, tx pgx.Tx, id uuid.UUID, _ int64) error {
	q := s.q.WithTx(tx)
	r, err := q.LockRun(ctx, id)
	if err != nil {
		return noRows(err, "run "+id.String())
	}
	if r.Status != string(RunDraft) && r.Status != string(RunApproved) {
		return invalidState(fmt.Sprintf("run is %s; only a draft or approved run can be published", r.Status))
	}
	if err := q.SupersedePublished(ctx, db.SupersedePublishedParams{ModelID: r.ModelID, RiskProfileID: r.RiskProfileID, RunID: r.ID}); err != nil {
		return err
	}
	return q.PublishRun(ctx, r.ID)
}

// Replay rebuilds a run's EvalInput from what was stored, so Evaluate can
// reproduce it.
func (s *Store) Replay(ctx context.Context, id uuid.UUID) (EvalInput, error) {
	r, err := s.q.GetRun(ctx, id)
	if err != nil {
		return EvalInput{}, noRows(err, "run "+id.String())
	}
	cv, err := s.Get(ctx, r.ModelKey, int(r.ConfigVersion))
	if err != nil {
		return EvalInput{}, err
	}
	prof, err := s.ProfileByID(ctx, r.RiskProfileID)
	if err != nil {
		return EvalInput{}, err
	}
	var rec RunRecord
	if err := json.Unmarshal(r.Params, &rec); err != nil {
		return EvalInput{}, err
	}
	in := EvalInput{AsOf: fromPGDate(r.AsOf), Cutoff: r.KnowledgeCutoff, Config: cv.Config, Profile: prof,
		Inputs: map[string]Input{}, Params: rec.Params, Overrides: rec.Overrides, History: rec.History}
	rows, err := s.q.RunValues(ctx, db.RunValuesParams{RunID: r.ID, Step: strPtr("input")})
	if err != nil {
		return EvalInput{}, err
	}
	for _, v := range rows {
		var si storedInputs
		if err := json.Unmarshal(v.Inputs, &si); err != nil {
			return EvalInput{}, err
		}
		inp := Input{Value: datapoints.Value{Num: v.ValueNum, Bool: v.ValueBool}}
		src, obs, _ := strings.Cut(si.Formula, " @")
		inp.Source = src
		if obs != "" {
			if d, err := civil.ParseDate(obs); err == nil {
				inp.ObservedAt = &d
			}
		}
		if len(si.Datapoints) > 0 {
			inp.DatapointID = si.Datapoints[0]
		}
		in.Inputs[strings.TrimPrefix(v.NodeKey, inputPrefix)] = inp
	}
	return in, nil
}

// PublishedHistory returns up to n published (or since superseded) runs of
// model before asOf, newest first.
func (s *Store) PublishedHistory(ctx context.Context, model string, asOf civil.Date, n int) ([]Run, error) {
	rows, err := s.q.PublishedBefore(ctx, db.PublishedBeforeParams{ModelKey: model, AsOf: pgDate(asOf), Lim: int32(n)}) //nolint:gosec // small
	if err != nil {
		return nil, err
	}
	out := make([]Run, 0, len(rows))
	for _, r := range rows {
		var rec RunRecord
		if err := json.Unmarshal(r.Params, &rec); err != nil {
			return nil, err
		}
		out = append(out, Run{UUID: r.Uuid, AsOf: fromPGDate(r.AsOf), Params: rec.Params})
	}
	return out, nil
}

// ---- overrides ---------------------------------------------------------------------

// ActiveOverrides returns model's overrides in force at asOf.
func (s *Store) ActiveOverrides(ctx context.Context, model string, asOf civil.Date) ([]NodeOverride, error) {
	rows, err := s.q.ActiveOverrides(ctx, db.ActiveOverridesParams{ModelKey: model, AsOf: pgDate(asOf)})
	if err != nil {
		return nil, err
	}
	out := make([]NodeOverride, 0, len(rows))
	for _, r := range rows {
		out = append(out, NodeOverride{ID: r.Uuid, NodeKey: r.NodeKey, AsOf: fromPGDate(r.AsOf),
			Value: datapoints.Value{Num: r.ValueNum, Bool: r.ValueBool}, Reason: r.Reason, ActorID: r.ActorID, ValidUntil: fromPGDatePtr(r.ValidUntil)})
	}
	return out, nil
}

// CreateOverride stores a node override.
func (s *Store) CreateOverride(ctx context.Context, tx pgx.Tx, model string, o NodeOverride) (NodeOverride, error) {
	if (o.Value.Num == nil) == (o.Value.Bool == nil) || o.Value.Cat != nil {
		return NodeOverride{}, apperr.New(apperr.ValidationFailed, "An override is a number or a boolean", apperr.Detail{Field: "value", Reason: "invalid_type"})
	}
	if strings.TrimSpace(o.Reason) == "" || o.NodeKey == "" {
		return NodeOverride{}, apperr.New(apperr.ValidationFailed, "Node and reason are required", apperr.Detail{Field: "reason", Reason: "required"})
	}
	q := s.q.WithTx(tx)
	mid, err := q.GetModelID(ctx, model)
	if err != nil {
		return NodeOverride{}, noRows(err, "model "+model)
	}
	id, err := q.InsertOverride(ctx, db.InsertOverrideParams{ModelID: mid, NodeKey: o.NodeKey, AsOf: pgDate(o.AsOf),
		ValueNum: o.Value.Num, ValueBool: o.Value.Bool, Reason: o.Reason, ActorID: o.ActorID, ValidUntil: pgDatePtr(o.ValidUntil)})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" {
		return NodeOverride{}, apperr.New(apperr.ValidationFailed, "Invalid override", apperr.Detail{Field: "valid_until", Reason: "before_as_of"})
	}
	if err != nil {
		return NodeOverride{}, err
	}
	o.ID = id
	return o, nil
}

// DeleteOverride soft-deletes an override.
func (s *Store) DeleteOverride(ctx context.Context, tx pgx.Tx, id uuid.UUID, actorID int64) error {
	n, err := s.q.WithTx(tx).DeleteOverride(ctx, db.DeleteOverrideParams{Uuid: id, ActorID: &actorID})
	if err != nil {
		return err
	}
	if n == 0 {
		return notFound("override " + id.String())
	}
	return nil
}

// ---- contract views ------------------------------------------------------------------
//
// The contracts share method names (Get, List, Active, Create), so the
// store exposes each as a view.

type profileView struct{ s *Store }

// Profiles returns the RiskProfiles view of the store.
func (s *Store) Profiles() RiskProfiles { return profileView{s} }

func (v profileView) Get(ctx context.Context, key string) (RiskProfile, error) {
	return v.s.GetProfile(ctx, key)
}
func (v profileView) List(ctx context.Context) ([]RiskProfile, error) { return v.s.ListProfiles(ctx) }
func (v profileView) Save(ctx context.Context, tx pgx.Tx, p RiskProfile) (RiskProfile, error) {
	return v.s.SaveProfile(ctx, tx, p)
}

type portfolioView struct{ s *Store }

// Portfolios returns the Portfolios view of the store.
func (s *Store) Portfolios() Portfolios { return portfolioView{s} }

func (v portfolioView) Get(ctx context.Context, key string) (Portfolio, error) {
	return v.s.Portfolio(ctx, key)
}

type runView struct{ s *Store }

// Runs returns the Runs view of the store.
func (s *Store) Runs() Runs { return runView{s} }

func (v runView) Create(ctx context.Context, tx pgx.Tx, r Run, in EvalInput, res Result) (Run, error) {
	return v.s.CreateRun(ctx, tx, r, in, res)
}
func (v runView) Get(ctx context.Context, id uuid.UUID) (Run, error) { return v.s.GetRun(ctx, id) }
func (v runView) List(ctx context.Context, f RunFilter) (paging.Page[Run], error) {
	return v.s.ListRuns(ctx, f)
}
func (v runView) Values(ctx context.Context, id uuid.UUID, step string) ([]Node, error) {
	return v.s.RunValues(ctx, id, step)
}
func (v runView) Allocation(ctx context.Context, id uuid.UUID) ([]AllocationLine, error) {
	return v.s.Allocation(ctx, id)
}
func (v runView) Approve(ctx context.Context, tx pgx.Tx, id uuid.UUID, actorID int64, note string) error {
	return v.s.Approve(ctx, tx, id, actorID, note)
}
func (v runView) Publish(ctx context.Context, tx pgx.Tx, id uuid.UUID, actorID int64) error {
	return v.s.Publish(ctx, tx, id, actorID)
}

type overrideView struct{ s *Store }

// Overrides returns the Overrides view of the store.
func (s *Store) Overrides() Overrides { return overrideView{s} }

func (v overrideView) Active(ctx context.Context, model string, asOf civil.Date) ([]NodeOverride, error) {
	return v.s.ActiveOverrides(ctx, model, asOf)
}
func (v overrideView) Create(ctx context.Context, tx pgx.Tx, model string, o NodeOverride) (NodeOverride, error) {
	return v.s.CreateOverride(ctx, tx, model, o)
}
func (v overrideView) Delete(ctx context.Context, tx pgx.Tx, id uuid.UUID, actorID int64) error {
	return v.s.DeleteOverride(ctx, tx, id, actorID)
}
