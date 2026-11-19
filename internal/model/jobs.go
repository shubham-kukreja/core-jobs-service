package model

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"cloud.google.com/go/civil"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
)

// Job kinds.
const (
	KindRun  = "model.run"
	KindVeto = "veto.evaluate"
)

// DefaultModel and DefaultPortfolio name the one model and portfolio CORE
// runs today.
const (
	DefaultModel     = "maaf"
	DefaultPortfolio = "core"
)

// JobDeps are what the model jobs need.
type JobDeps struct {
	DB     database.TxBeginner
	Store  *Store
	Engine Engine
	Loader InputLoader
	Reader datapoints.Reader
	Writer datapoints.Writer
	Clock  clock.Clock
}

// ---- model.run -----------------------------------------------------------------

// RunArgs are model.run's arguments.
type RunArgs struct {
	Model  string     `json:"model"`
	AsOf   civil.Date `json:"as_of"`
	Cutoff *time.Time `json:"cutoff,omitempty"`
	// ConfigVersion runs a specific (e.g. draft) config: a what-if. Default:
	// the active config.
	ConfigVersion *int `json:"config_version,omitempty"`
	// RiskProfile defaults to the default portfolio's profile.
	RiskProfile string `json:"risk_profile,omitempty"`
	Params      Params `json:"params"`
	// Publish makes the run the published allocation (admin only).
	Publish bool `json:"publish"`
}

func (RunArgs) Kind() string { return KindRun }

// MinRole raises the role needed to submit a publishing run.
func (a RunArgs) MinRole() string {
	if a.Publish {
		return "admin"
	}
	return ""
}

// RunSpec is model.run's static definition.
var RunSpec = jobs.Spec{
	Kind:            KindRun,
	Queue:           jobs.QueueModel,
	Triggers:        []jobs.TriggerType{jobs.TriggerSchedule, jobs.TriggerUser},
	UserTriggerable: true,
	Roles:           []string{"analyst", "admin"},
	MaxAttempts:     3,
	Timeout:         5 * time.Minute,
	ArgsVersion:     1,
	Unique:          &jobs.UniqueOpts{ByArgs: []string{"model", "as_of", "config_version", "risk_profile", "params", "publish"}, Period: 10 * time.Minute},
}

// permanent wraps errors a retry cannot fix: bad config, bad params,
// missing model or profile.
func permanent(err error) error {
	switch apperr.CodeOf(err) {
	case apperr.InvalidParams, apperr.ValidationFailed, apperr.NotFound, apperr.Conflict:
		return jobs.Permanent(err)
	}
	return err
}

// RunModel evaluates and stores one run. It is the model.run handler's body.
func RunModel(ctx context.Context, d JobDeps, a RunArgs, jobRunID, userID *int64) (Run, Result, error) {
	if a.Model == "" {
		a.Model = DefaultModel
	}
	var cv ConfigVersion
	var err error
	if a.ConfigVersion != nil {
		cv, err = d.Store.Get(ctx, a.Model, *a.ConfigVersion)
	} else {
		cv, err = d.Store.Active(ctx, a.Model)
	}
	if err != nil {
		return Run{}, Result{}, err
	}
	if a.Publish && cv.Status != ConfigActive {
		return Run{}, Result{}, apperr.New(apperr.InvalidParams, fmt.Sprintf("Config v%d is %s; only the active config can be published", cv.Version, cv.Status),
			apperr.Detail{Field: "publish", Reason: "config_not_active"})
	}
	prof, err := resolveProfile(ctx, d.Store, a)
	if err != nil {
		return Run{}, Result{}, err
	}
	req := LoadRequest{Model: a.Model, AsOf: a.AsOf, Config: cv.Config, Profile: prof, Params: a.Params}
	if a.Cutoff != nil {
		req.Cutoff = *a.Cutoff
	}
	in, err := d.Loader.Load(ctx, req)
	if err != nil {
		return Run{}, Result{}, err
	}
	res, err := d.Engine.Evaluate(ctx, in)
	if err != nil {
		return Run{}, Result{}, err
	}
	run := Run{ModelKey: a.Model, ConfigVersion: cv.Version, Status: RunDraft, JobRunID: jobRunID, CreatedBy: userID}
	if a.Publish {
		run.Status = RunPublished
	}
	err = database.InTx(ctx, d.DB, func(tx pgx.Tx) error {
		run, err = d.Store.CreateRun(ctx, tx, run, in, res)
		return err
	})
	return run, res, err
}

func resolveProfile(ctx context.Context, s *Store, a RunArgs) (RiskProfile, error) {
	if a.RiskProfile != "" {
		return s.GetProfile(ctx, a.RiskProfile)
	}
	p, err := s.Portfolio(ctx, DefaultPortfolio)
	if err != nil {
		return RiskProfile{}, err
	}
	if p.ModelKey != a.Model {
		return RiskProfile{}, apperr.New(apperr.InvalidParams, "Name a risk_profile: the default portfolio runs model "+p.ModelKey,
			apperr.Detail{Field: "risk_profile", Reason: "required"})
	}
	pinned, err := s.ProfileByID(ctx, p.RiskProfileID)
	if err != nil {
		return RiskProfile{}, err
	}
	return s.GetProfile(ctx, pinned.Key) // its latest version
}

// RegisterRun registers model.run.
func RegisterRun(r *jobs.Registry, d JobDeps) {
	jobs.Register(r, RunSpec, jobs.HandlerFunc[RunArgs](func(ctx context.Context, rc *jobs.RunCtx, a RunArgs) (jobs.Outcome, error) {
		id := rc.RunID
		run, res, err := RunModel(ctx, d, a, &id, rc.UserID)
		if err != nil {
			return jobs.Outcome{}, permanent(err)
		}
		warnings := res.Warnings
		if len(warnings) > 50 {
			warnings = warnings[:50]
		}
		sum := map[string]any{
			"run_id": run.UUID.String(), "status": string(run.Status), "as_of": run.AsOf.String(),
			"config_version": run.ConfigVersion, "risk_profile": run.RiskProfileKey,
			"warnings": len(res.Warnings), "warning_list": warnings, "derivation_hash": hex.EncodeToString(run.DerivationHash),
		}
		return jobs.Outcome{Summary: toMap(sum)}, nil
	}))
}

func toMap(v map[string]any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		return v
	}
	return out
}

// ---- veto.evaluate -----------------------------------------------------------------

// VetoArgs are veto.evaluate's arguments.
type VetoArgs struct {
	AsOf  *civil.Date `json:"as_of,omitempty"` // default: today
	Model string      `json:"model,omitempty"` // default: maaf
}

func (VetoArgs) Kind() string { return KindVeto }

// VetoSpec is veto.evaluate's static definition.
var VetoSpec = jobs.Spec{
	Kind:            KindVeto,
	Queue:           jobs.QueueModel,
	Triggers:        []jobs.TriggerType{jobs.TriggerEvent, jobs.TriggerSchedule, jobs.TriggerUser},
	UserTriggerable: true,
	Roles:           []string{"analyst", "admin"},
	MaxAttempts:     3,
	Timeout:         2 * time.Minute,
	ArgsVersion:     1,
	Unique:          &jobs.UniqueOpts{ByArgs: []string{"as_of", "model"}, Period: 10 * time.Minute},
}

// VetoSeries is the series holding a bucket's veto state.
func VetoSeries(bucket string) string { return "veto." + bucket }

// VetoResult is what EvaluateVetoes found and wrote.
type VetoResult struct {
	AsOf         civil.Date `json:"as_of"`
	Active       []string   `json:"active"`
	NewlyActive  []string   `json:"newly_active"`
	Written      int        `json:"written"`
	NoTriggerFor []string   `json:"no_trigger_data,omitempty"`
}

// EvaluateVetoes maps trigger datapoints to buckets with the active
// config's veto maps (the same rule the engine's veto step applies) and
// writes veto.<bucket> = any trigger true, with lineage to the triggers.
// Buckets with no trigger data at all are not written.
func EvaluateVetoes(ctx context.Context, d JobDeps, a VetoArgs, jobRunID *int64) (VetoResult, error) {
	model := a.Model
	if model == "" {
		model = DefaultModel
	}
	asOf := civil.DateOf(d.Clock.Now())
	if a.AsOf != nil {
		asOf = *a.AsOf
	}
	cv, err := d.Store.Active(ctx, model)
	if err != nil {
		return VetoResult{}, err
	}
	res := VetoResult{AsOf: asOf, Active: []string{}, NewlyActive: []string{}}
	o := datapoints.ReadOpts{Cutoff: d.Clock.Now()}
	latest := func(key string, upTo civil.Date) (*datapoints.Resolved, error) {
		rows, err := d.Reader.History(ctx, key, upTo, TriggerLookback, o)
		if errors.Is(err, catalog.ErrNotFound) || len(rows) == 0 {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &rows[len(rows)-1], nil
	}
	var pts []datapoints.Point
	for _, b := range vetoBuckets(cv.Config) {
		var ids []int64
		var known time.Time
		var on, seen bool
		for _, key := range b.triggers {
			r, err := latest(key, asOf)
			if err != nil {
				return res, err
			}
			if r == nil {
				continue
			}
			seen = true
			ids = append(ids, r.ID)
			if r.KnownAt.After(known) {
				known = r.KnownAt
			}
			if r.Value.Bool != nil && *r.Value.Bool {
				on = true
			}
		}
		if !seen {
			res.NoTriggerFor = append(res.NoTriggerFor, b.bucket)
			continue
		}
		if on {
			res.Active = append(res.Active, b.bucket)
			prev, err := latest(VetoSeries(b.bucket), asOf.AddDays(-1))
			if err != nil {
				return res, err
			}
			if prev == nil || prev.Value.Bool == nil || !*prev.Value.Bool {
				res.NewlyActive = append(res.NewlyActive, b.bucket)
			}
		}
		slices.Sort(ids)
		pts = append(pts, datapoints.Point{SeriesKey: VetoSeries(b.bucket), ObsDate: asOf, Value: datapoints.Bool(on), KnownAt: known, Inputs: ids})
	}
	if len(pts) == 0 {
		return res, nil
	}
	meta := datapoints.WriteMeta{Source: "computed", JobRunID: jobRunID, Origin: datapoints.OriginCascade}
	err = database.InTx(ctx, d.DB, func(tx pgx.Tx) error {
		w, err := d.Writer.Write(ctx, tx, meta, pts)
		if err != nil {
			return err
		}
		// A changed veto whose triggers' known_at equals the stored row's
		// (more triggers arrived in the same batch) is knowable now, as in
		// the cascade.
		var retry []datapoints.Point
		for _, rj := range w.Rejected {
			if rj.Reason != datapoints.ReasonDuplicateKnownAt {
				return apperr.New(apperr.InvalidParams, fmt.Sprintf("veto %s rejected: %s %s", rj.SeriesKey, rj.Reason, rj.Message))
			}
			p := pts[rj.Index]
			p.KnownAt = d.Clock.Now()
			retry = append(retry, p)
		}
		res.Written = w.Written
		if len(retry) == 0 {
			return nil
		}
		w2, err := d.Writer.Write(ctx, tx, meta, retry)
		if err != nil {
			return err
		}
		if len(w2.Rejected) > 0 {
			rj := w2.Rejected[0]
			return apperr.New(apperr.InvalidParams, fmt.Sprintf("veto %s rejected: %s %s", rj.SeriesKey, rj.Reason, rj.Message))
		}
		res.Written += w2.Written
		return nil
	})
	return res, err
}

type vetoBucket struct {
	bucket   string
	triggers []string
}

// vetoBuckets lists every bucket and sector with veto templates, expanded.
func vetoBuckets(c Config) []vetoBucket {
	var out []vetoBucket
	add := func(b string, vetoes map[string][]string) {
		var ts []string
		for _, t := range append(slices.Clone(vetoes["*"]), vetoes[b]...) {
			ts = append(ts, expand(t, b, ""))
		}
		if len(ts) > 0 {
			sort.Strings(ts)
			out = append(out, vetoBucket{b, slices.Compact(ts)})
		}
	}
	for _, name := range scorecardOrder(c) {
		sc := c.Scorecards[name]
		for _, b := range sc.Buckets {
			add(b, sc.Vetoes)
		}
	}
	if sat := c.SectorSatellite; sat != nil {
		for _, sec := range sat.Sectors {
			add(sec, sat.Vetoes)
		}
	}
	return out
}

// RegisterVeto registers veto.evaluate.
func RegisterVeto(r *jobs.Registry, d JobDeps) {
	jobs.Register(r, VetoSpec, jobs.HandlerFunc[VetoArgs](func(ctx context.Context, rc *jobs.RunCtx, a VetoArgs) (jobs.Outcome, error) {
		id := rc.RunID
		res, err := EvaluateVetoes(ctx, d, a, &id)
		if err != nil {
			return jobs.Outcome{}, permanent(err)
		}
		for _, b := range res.NewlyActive {
			rc.Log.Warn("veto newly active", "bucket", b, "as_of", res.AsOf.String())
		}
		b, _ := json.Marshal(res)
		var sum map[string]any
		_ = json.Unmarshal(b, &sum)
		return jobs.Outcome{Summary: sum}, nil
	}))
}

// VetoSubscriber enqueues veto.evaluate when trigger datapoints are written.
type VetoSubscriber struct{}

func (VetoSubscriber) Topic() string { return datapoints.TopicWritten }

func (VetoSubscriber) Handle(_ context.Context, ev jobs.Event) ([]jobs.Args, error) {
	var w datapoints.WrittenEvent
	if err := json.Unmarshal(ev.Payload, &w); err != nil {
		return nil, fmt.Errorf("model: decode %s: %w", ev.Topic, err)
	}
	if !slices.Contains(w.Layers, string(catalog.LayerTrigger)) {
		return nil, nil
	}
	d := w.MaxDate
	return []jobs.Args{VetoArgs{AsOf: &d}}, nil
}
