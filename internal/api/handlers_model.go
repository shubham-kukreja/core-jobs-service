package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/civil"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/model"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
)

// ---- shared shapes -----------------------------------------------------------------

type userRefJSON struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

func (s *Server) userRef(ctx context.Context, id *int64) *userRefJSON {
	if id == nil {
		return nil
	}
	u, err := s.users.Get(ctx, *id)
	if err != nil {
		return nil
	}
	return &userRefJSON{ID: u.UUID, Name: u.Name}
}

func pathModel(r *http.Request) (string, error) {
	m := chi.URLParam(r, "model")
	if !keyRE.MatchString(m) {
		return "", apperr.New(apperr.NotFound, "Not found")
	}
	return m, nil
}

func nodeValue(n model.Node) any {
	switch {
	case n.Num != nil:
		return *n.Num
	case n.Bool != nil:
		return *n.Bool
	}
	return nil
}

func dec6(d decimal.Decimal) string { return d.StringFixed(6) }

// ---- configs -------------------------------------------------------------------------

type modelConfigJSON struct {
	Model         string       `json:"model"`
	Version       int          `json:"version"`
	Status        string       `json:"status"`
	Config        model.Config `json:"config"`
	Note          *string      `json:"note"`
	EffectiveFrom *civil.Date  `json:"effective_from"`
	CreatedBy     *userRefJSON `json:"created_by"`
	CreatedAt     time.Time    `json:"created_at"`
}

func (s *Server) configJSON(ctx context.Context, cv model.ConfigVersion) modelConfigJSON {
	out := modelConfigJSON{Model: cv.ModelKey, Version: cv.Version, Status: string(cv.Status), Config: cv.Config,
		EffectiveFrom: cv.EffectiveFrom, CreatedBy: s.userRef(ctx, cv.CreatedBy), CreatedAt: cv.CreatedAt.UTC()}
	if cv.Note != "" {
		out.Note = &cv.Note
	}
	return out
}

type configValidationJSON struct {
	OK     bool            `json:"ok"`
	Errors []apperr.Detail `json:"errors"`
}

func validationOf(err error) (configValidationJSON, error) {
	if err == nil {
		return configValidationJSON{OK: true, Errors: []apperr.Detail{}}, nil
	}
	if e, ok := apperr.As(err); ok && e.Code == apperr.ValidationFailed {
		d := e.Details
		if d == nil {
			d = []apperr.Detail{}
		}
		return configValidationJSON{OK: false, Errors: d}, nil
	}
	return configValidationJSON{}, err
}

func (s *Server) listModelConfigs(w http.ResponseWriter, r *http.Request) {
	m, err := pathModel(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, err := s.models.List(r.Context(), m)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if len(list) == 0 {
		s.writeError(w, r, apperr.New(apperr.NotFound, "Model not found"))
		return
	}
	out := make([]modelConfigJSON, 0, len(list))
	for _, cv := range list {
		out = append(out, s.configJSON(r.Context(), cv))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// configVersion resolves {version}: a number or "active".
func (s *Server) configVersion(r *http.Request, m string) (model.ConfigVersion, error) {
	v := chi.URLParam(r, "version")
	if v == "active" {
		return s.models.Active(r.Context(), m)
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 999999 {
		return model.ConfigVersion{}, apperr.New(apperr.NotFound, "Not found")
	}
	return s.models.Get(r.Context(), m, n)
}

func (s *Server) getModelConfig(w http.ResponseWriter, r *http.Request) {
	m, err := pathModel(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	cv, err := s.configVersion(r, m)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.configJSON(r.Context(), cv))
}

var (
	patchPathRE = regexp.MustCompile(`^(/[^/]{1,64}){1,10}$`)
	scorecardRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

type configCreate struct {
	BaseVersion int             `json:"base_version"`
	Patch       []model.PatchOp `json:"patch"`
	Note        string          `json:"note"`
}

func (s *Server) createModelConfig(w http.ResponseWriter, r *http.Request) {
	m, err := pathModel(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	body, err := readBody(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var in configCreate
	if err := decodeJSON(r, body, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	var p problems
	if in.BaseVersion < 1 {
		p.add("base_version", "required")
	}
	if len(in.Patch) == 0 || len(in.Patch) > 200 {
		p.add("patch", "out_of_range")
	}
	for i, op := range in.Patch {
		p.oneOf(fmt.Sprintf("patch[%d].op", i), op.Op, "add", "remove", "replace", "move", "copy", "test")
		if !patchPathRE.MatchString(op.Path) {
			p.addAt(i, "patch.path", "invalid_pointer")
		}
		if op.From != "" && !patchPathRE.MatchString(op.From) {
			p.addAt(i, "patch.from", "invalid_pointer")
		}
	}
	p.text("note", in.Note, 0, 2000)
	if err := p.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.mutate(w, r, body, func(ctx context.Context, tx pgx.Tx) (txResult, error) {
		cv, err := s.models.Create(ctx, tx, m, in.BaseVersion, in.Patch, in.Note, mustPrincipal(ctx).UserID)
		if err != nil {
			return txResult{}, err
		}
		val, err := validationOf(s.models.ValidateConfig(ctx, cv.Config))
		if err != nil {
			return txResult{}, err
		}
		if err := s.audit.Record(ctx, tx, auditEntry(r, "model_config", fmt.Sprintf("%s/%d", m, cv.Version), "create", nil,
			map[string]any{"base_version": in.BaseVersion, "patch": in.Patch, "note": in.Note, "valid": val.OK})); err != nil {
			return txResult{}, err
		}
		return txResult{Status: http.StatusCreated, Body: struct {
			Version    int                  `json:"version"`
			Status     model.ConfigStatus   `json:"status"`
			Validation configValidationJSON `json:"validation"`
		}{cv.Version, cv.Status, val}}, nil
	})
}

func (s *Server) diffModelConfigs(w http.ResponseWriter, r *http.Request) {
	m, err := pathModel(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var p problems
	from, err1 := strconv.Atoi(r.URL.Query().Get("from"))
	to, err2 := strconv.Atoi(r.URL.Query().Get("to"))
	if err1 != nil || from < 1 {
		p.add("from", "required")
	}
	if err2 != nil || to < 1 {
		p.add("to", "required")
	}
	if err := p.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	ops, err := s.models.Diff(r.Context(), m, from, to)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "patch": ops})
}

func (s *Server) validateModelConfig(w http.ResponseWriter, r *http.Request) {
	m, err := pathModel(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	cv, err := s.configVersion(r, m)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	val, err := validationOf(s.models.ValidateConfig(r.Context(), cv.Config))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, val)
}

func (s *Server) activateModelConfig(w http.ResponseWriter, r *http.Request) {
	m, err := pathModel(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	cv, err := s.configVersion(r, m)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.mutate(w, r, nil, func(ctx context.Context, tx pgx.Tx) (txResult, error) {
		before := cv.Status
		if err := s.models.Activate(ctx, tx, m, cv.Version); err != nil {
			return txResult{}, err
		}
		cv.Status = model.ConfigActive
		if err := s.audit.Record(ctx, tx, auditEntry(r, "model_config", fmt.Sprintf("%s/%d", m, cv.Version), "activate",
			map[string]any{"status": before}, map[string]any{"status": cv.Status})); err != nil {
			return txResult{}, err
		}
		return txResult{Status: http.StatusOK, Body: s.configJSON(ctx, cv)}, nil
	})
}

// ---- overrides -----------------------------------------------------------------------

type overrideInput struct {
	NodeKey    string          `json:"node_key"`
	AsOf       string          `json:"as_of"`
	Value      json.RawMessage `json:"value"`
	Reason     string          `json:"reason"`
	ValidUntil string          `json:"valid_until"`
}

type overrideJSON struct {
	ID         uuid.UUID    `json:"id"`
	NodeKey    string       `json:"node_key"`
	AsOf       civil.Date   `json:"as_of"`
	Value      any          `json:"value"`
	Reason     string       `json:"reason"`
	ValidUntil *civil.Date  `json:"valid_until"`
	Actor      *userRefJSON `json:"actor"`
	CreatedAt  time.Time    `json:"created_at"`
}

func (s *Server) createNodeOverride(w http.ResponseWriter, r *http.Request) {
	m, err := pathModel(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	body, err := readBody(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var in overrideInput
	if err := decodeJSON(r, body, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	var p problems
	p.key("node_key", in.NodeKey)
	p.text("reason", strings.TrimSpace(in.Reason), 1, 2000)
	o := model.NodeOverride{NodeKey: in.NodeKey, Reason: in.Reason, ActorID: mustPrincipal(r.Context()).UserID}
	if d, err := civil.ParseDate(in.AsOf); err != nil {
		p.add("as_of", "invalid_date")
	} else {
		o.AsOf = d
	}
	if in.ValidUntil != "" {
		d, err := civil.ParseDate(in.ValidUntil)
		switch {
		case err != nil:
			p.add("valid_until", "invalid_date")
		case d.Before(o.AsOf):
			p.add("valid_until", "before_as_of")
		default:
			o.ValidUntil = &d
		}
	}
	var num float64
	var b bool
	switch {
	case json.Unmarshal(in.Value, &b) == nil && (string(in.Value) == "true" || string(in.Value) == "false"):
		o.Value = datapoints.Bool(b)
	case json.Unmarshal(in.Value, &num) == nil && !math.IsNaN(num) && !math.IsInf(num, 0):
		o.Value = datapoints.Num(num)
	default:
		p.add("value", "invalid_type")
	}
	if err := p.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.mutate(w, r, body, func(ctx context.Context, tx pgx.Tx) (txResult, error) {
		saved, err := s.models.CreateOverride(ctx, tx, m, o)
		if err != nil {
			return txResult{}, err
		}
		out := overrideJSON{ID: saved.ID, NodeKey: saved.NodeKey, AsOf: saved.AsOf, Value: nodeValue(model.Node{Num: saved.Value.Num, Bool: saved.Value.Bool}),
			Reason: saved.Reason, ValidUntil: saved.ValidUntil, Actor: s.userRef(ctx, &saved.ActorID), CreatedAt: saved.CreatedAt.UTC()}
		if err := s.audit.Record(ctx, tx, auditEntry(r, "node_override", saved.ID.String(), "create", nil, out)); err != nil {
			return txResult{}, err
		}
		return txResult{Status: http.StatusCreated, Body: out}, nil
	})
}

func (s *Server) deleteNodeOverride(w http.ResponseWriter, r *http.Request) {
	if _, err := pathModel(r); err != nil {
		s.writeError(w, r, err)
		return
	}
	id, err := pathUUID(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.mutate(w, r, nil, func(ctx context.Context, tx pgx.Tx) (txResult, error) {
		if err := s.models.DeleteOverride(ctx, tx, id, mustPrincipal(ctx).UserID); err != nil {
			return txResult{}, err
		}
		if err := s.audit.Record(ctx, tx, auditEntry(r, "node_override", id.String(), "delete", nil, nil)); err != nil {
			return txResult{}, err
		}
		return txResult{Status: http.StatusNoContent}, nil
	})
}

// ---- runs ------------------------------------------------------------------------------

type runJSON struct {
	ID              uuid.UUID       `json:"id"`
	Model           string          `json:"model"`
	ConfigVersion   int             `json:"config_version"`
	RiskProfile     string          `json:"risk_profile"`
	AsOf            civil.Date      `json:"as_of"`
	KnowledgeCutoff time.Time       `json:"knowledge_cutoff"`
	Params          model.Params    `json:"params"`
	Status          string          `json:"status"`
	Warnings        []model.Warning `json:"warnings"`
	Summary         *runSummaryJSON `json:"summary,omitempty"`
	JobRunID        *uuid.UUID      `json:"job_run_id"`
	ApprovedBy      *userRefJSON    `json:"approved_by"`
	ApprovedAt      *time.Time      `json:"approved_at"`
	CreatedAt       time.Time       `json:"created_at"`
}

type runSummaryJSON struct {
	L1               []l1RowJSON         `json:"l1"`
	SectorQualifiers []sectorSummaryJSON `json:"sector_qualifiers"`
}

type l1RowJSON struct {
	Bucket    string  `json:"bucket"`
	Composite float64 `json:"composite"`
	Veto      bool    `json:"veto"`
	Tilt      float64 `json:"tilt"`
	Final     float64 `json:"final"`
	Neutral   float64 `json:"neutral"`
}

type sectorSummaryJSON struct {
	Sector    string  `json:"sector"`
	Composite float64 `json:"composite"`
}

func (s *Server) runJSON(ctx context.Context, run model.Run, withSummary bool) (runJSON, error) {
	out := runJSON{ID: run.UUID, Model: run.ModelKey, ConfigVersion: run.ConfigVersion, RiskProfile: run.RiskProfileKey,
		AsOf: run.AsOf, KnowledgeCutoff: run.KnowledgeCutoff.UTC(), Params: run.Params, Status: string(run.Status),
		Warnings: run.Warnings, ApprovedBy: s.userRef(ctx, run.ApprovedBy), ApprovedAt: run.ApprovedAt, CreatedAt: run.CreatedAt.UTC()}
	if out.Warnings == nil {
		out.Warnings = []model.Warning{}
	}
	if run.JobRunID != nil {
		m, err := s.jobs.Runs.UUIDs(ctx, []int64{*run.JobRunID})
		if err != nil {
			return out, err
		}
		if u, ok := m[*run.JobRunID]; ok {
			out.JobRunID = &u
		}
	}
	if !withSummary {
		return out, nil
	}
	sum, err := s.runSummary(ctx, run)
	if err != nil {
		return out, err
	}
	out.Summary = sum
	return out, nil
}

func (s *Server) runSummary(ctx context.Context, run model.Run) (*runSummaryJSON, error) {
	cv, err := s.models.Get(ctx, run.ModelKey, run.ConfigVersion)
	if err != nil {
		return nil, err
	}
	prof, err := s.models.ProfileByID(ctx, run.RiskProfileID)
	if err != nil {
		return nil, err
	}
	vals, err := s.models.RunValues(ctx, run.UUID, "")
	if err != nil {
		return nil, err
	}
	n := map[string]model.Node{}
	for _, v := range vals {
		n[v.Key] = v
	}
	f := func(k string) float64 {
		if v, ok := n[k]; ok && v.Num != nil {
			return *v.Num
		}
		return 0
	}
	b := func(k string) bool { v, ok := n[k]; return ok && v.Bool != nil && *v.Bool }
	out := &runSummaryJSON{L1: []l1RowJSON{}, SectorQualifiers: []sectorSummaryJSON{}}
	for _, sc := range cv.Config.Scorecards {
		if sc.Parent != "" {
			continue
		}
		for _, g := range sc.Buckets {
			out.L1 = append(out.L1, l1RowJSON{Bucket: g, Composite: f(g + "::composite"), Veto: b(g + "::veto"),
				Tilt: f(g + "::tilt"), Final: f(g + "::weight"), Neutral: prof.NeutralWeights[g].InexactFloat64()})
		}
	}
	if sat := cv.Config.SectorSatellite; sat != nil {
		for _, sec := range sat.Sectors {
			if b(sec + "::qualifies") {
				out.SectorQualifiers = append(out.SectorQualifiers, sectorSummaryJSON{Sector: sec, Composite: f(sec + "::composite")})
			}
		}
	}
	return out, nil
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var p problems
	f := model.RunFilter{Model: q.Get("model"), Status: model.RunStatus(q.Get("status"))}
	if f.Model != "" {
		p.key("model", f.Model)
	}
	if f.Status != "" {
		p.oneOf("status", string(f.Status), "running", "draft", "approved", "published", "superseded", "failed")
	}
	if v := q.Get("from"); v != "" {
		if d, err := civil.ParseDate(v); err != nil {
			p.add("from", "invalid_date")
		} else {
			f.From = &d
		}
	}
	if err := p.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	scope := "runs:" + f.Model + ":" + string(f.Status) + ":" + q.Get("from")
	page, err := queryPage(r, s, scope)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	f.Page = page
	res, err := s.models.ListRuns(r.Context(), f)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]runJSON, 0, len(res.Items))
	for _, run := range res.Items {
		j, err := s.runJSON(r.Context(), run, false)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "next_cursor": s.cursors.Encode(scope, res.Next)})
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	run, err := s.models.GetRun(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out, err := s.runJSON(r.Context(), run, true)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

var runSteps = []string{"composite", "veto", "tilt", "sector_qualify", "constraints", "normalize", "sleeve", "rollup"}

func (s *Server) listRunValues(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	step, scorecard := r.URL.Query().Get("step"), r.URL.Query().Get("scorecard")
	var p problems
	if step != "" {
		p.oneOf("step", step, runSteps...)
	}
	if scorecard != "" && !scorecardRE.MatchString(scorecard) {
		p.add("scorecard", "invalid_format")
	}
	if err := p.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	run, err := s.models.GetRun(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var buckets []string
	if scorecard != "" {
		cv, err := s.models.Get(r.Context(), run.ModelKey, run.ConfigVersion)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		if scorecard == "sector_satellite" && cv.Config.SectorSatellite != nil {
			buckets = append(buckets, cv.Config.SectorSatellite.Sectors...)
			buckets = append(buckets, cv.Config.SectorSatellite.Parent)
		} else if sc, ok := cv.Config.Scorecards[scorecard]; ok {
			buckets = sc.Buckets
		} else {
			s.writeError(w, r, apperr.New(apperr.ValidationFailed, "Unknown scorecard", apperr.Detail{Field: "scorecard", Reason: "unknown"}))
			return
		}
	}
	vals, err := s.models.RunValues(r.Context(), id, step)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	type runValueJSON struct {
		Node       string `json:"node"`
		Value      any    `json:"value"`
		Step       string `json:"step"`
		IsOverride bool   `json:"is_override"`
	}
	out := make([]runValueJSON, 0, len(vals))
	for _, v := range vals {
		if v.Step == "input" {
			continue
		}
		if buckets != nil {
			b, _, _ := strings.Cut(v.Key, "::")
			if !slices.Contains(buckets, b) {
				continue
			}
		}
		out = append(out, runValueJSON{Node: v.Key, Value: nodeValue(v), Step: v.Step, IsOverride: v.IsOverride})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) getRunValueLineage(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	key := chi.URLParam(r, "node_key")
	if !keyRE.MatchString(key) {
		s.writeError(w, r, apperr.New(apperr.NotFound, "Not found"))
		return
	}
	vals, err := s.models.RunValues(r.Context(), id, "")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	byKey := map[string]model.Node{}
	for _, v := range vals {
		byKey[v.Key] = v
	}
	n, ok := byKey[key]
	if !ok || n.Step == "input" {
		s.writeError(w, r, apperr.New(apperr.NotFound, "Node not found"))
		return
	}
	nodes := map[string]any{}
	for _, k := range n.Inputs.Nodes {
		if v, ok := byKey[k]; ok {
			nodes[k] = nodeValue(v)
		} else {
			nodes[k] = nil
		}
	}
	dps := make([]map[string]any, 0, len(n.Inputs.Datapoints))
	for _, dpID := range n.Inputs.Datapoints {
		dp, err := s.reader.Get(r.Context(), dpID)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		dps = append(dps, map[string]any{"id": dp.ID, "series": dp.SeriesKey, "value": valueOut(dp.Value)})
	}
	params := n.Inputs.Params
	if params == nil {
		params = map[string]float64{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"node": n.Key, "value": nodeValue(n), "step": n.Step, "formula": n.Formula, "is_override": n.IsOverride,
		"inputs": map[string]any{"nodes": nodes, "params": params, "datapoints": dps},
	})
}

type allocationLineJSON struct {
	LineItem        string `json:"line_item"`
	Group           string `json:"group"`
	GroupWeight     string `json:"group_weight"`
	WithinGroup     string `json:"within_group"`
	PortfolioWeight string `json:"portfolio_weight"`
	Neutral         string `json:"neutral"`
}

func (s *Server) allocationJSON(ctx context.Context, run model.Run) (map[string]any, error) {
	lines, err := s.models.Allocation(ctx, run.UUID)
	if err != nil {
		return nil, err
	}
	if cv, err := s.models.Get(ctx, run.ModelKey, run.ConfigVersion); err == nil {
		order := model.LineOrder(cv.Config)
		slices.SortStableFunc(lines, func(a, b model.AllocationLine) int {
			return slices.Index(order, a.LineItem) - slices.Index(order, b.LineItem)
		})
	}
	total := decimal.Zero
	out := make([]allocationLineJSON, 0, len(lines))
	for _, l := range lines {
		total = total.Add(l.PortfolioWeight)
		out = append(out, allocationLineJSON{LineItem: l.LineItem, Group: l.Group, GroupWeight: dec6(l.GroupWeight),
			WithinGroup: dec6(l.WithinGroup), PortfolioWeight: dec6(l.PortfolioWeight), Neutral: dec6(l.NeutralWeight)})
	}
	return map[string]any{"run_id": run.UUID, "profile": run.RiskProfileKey, "as_of": run.AsOf, "lines": out, "total": dec6(total)}, nil
}

func (s *Server) getRunAllocations(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	prof := r.URL.Query().Get("profile")
	if prof != "" {
		var p problems
		p.key("profile", prof)
		if err := p.err(); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	run, err := s.models.GetRun(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if prof != "" && prof != run.RiskProfileKey {
		s.writeError(w, r, apperr.New(apperr.NotFound, "The run has no allocation for profile "+prof))
		return
	}
	out, err := s.allocationJSON(r.Context(), run)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getCurrentAllocation(w http.ResponseWriter, r *http.Request) {
	m, prof := r.URL.Query().Get("model"), r.URL.Query().Get("profile")
	var p problems
	p.key("model", m)
	p.key("profile", prof)
	if err := p.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	run, err := s.models.CurrentRun(r.Context(), m, prof)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out, err := s.allocationJSON(r.Context(), run)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) compareRuns(w http.ResponseWriter, r *http.Request) {
	var p problems
	a, errA := uuid.Parse(r.URL.Query().Get("a"))
	b, errB := uuid.Parse(r.URL.Query().Get("b"))
	if errA != nil {
		p.add("a", "invalid_uuid")
	}
	if errB != nil {
		p.add("b", "invalid_uuid")
	}
	if err := p.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	weights := func(ctx context.Context, id uuid.UUID) (map[string]decimal.Decimal, map[string]model.Node, error) {
		lines, err := s.models.Allocation(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		vals, err := s.models.RunValues(ctx, id, "")
		if err != nil {
			return nil, nil, err
		}
		wm := map[string]decimal.Decimal{}
		for _, l := range lines {
			wm[l.LineItem] = l.PortfolioWeight
		}
		nm := map[string]model.Node{}
		for _, v := range vals {
			if v.Step != "input" {
				nm[v.Key] = v
			}
		}
		return wm, nm, nil
	}
	wa, na, err := weights(r.Context(), a)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	wb, nb, err := weights(r.Context(), b)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	type moveJSON struct {
		Node  string  `json:"node"`
		From  float64 `json:"from"`
		To    float64 `json:"to"`
		Delta float64 `json:"delta"`
	}
	type nodeChangeJSON struct {
		Node string `json:"node"`
		A    any    `json:"a"`
		B    any    `json:"b"`
	}
	lineKeys := unionKeys(wa, wb)
	moves := make([]moveJSON, 0, len(lineKeys))
	for _, k := range lineKeys {
		from, to := wa[k], wb[k]
		if from.Equal(to) {
			continue
		}
		moves = append(moves, moveJSON{Node: k, From: from.InexactFloat64(), To: to.InexactFloat64(), Delta: to.Sub(from).InexactFloat64()})
	}
	nodeKeys := unionKeys(na, nb)
	changed := make([]nodeChangeJSON, 0)
	for _, k := range nodeKeys {
		va, vb := nodeValue(na[k]), nodeValue(nb[k])
		if va == vb {
			continue
		}
		changed = append(changed, nodeChangeJSON{Node: k, A: va, B: vb})
	}
	writeJSON(w, http.StatusOK, map[string]any{"a": a, "b": b, "moves": moves, "nodes": changed})
}

func unionKeys[V any](a, b map[string]V) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range []map[string]V{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	slices.Sort(out)
	return out
}

type approveInput struct {
	Note string `json:"note"`
}

func (s *Server) approveRun(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	body, err := readBody(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var in approveInput
	if err := decodeJSON(r, body, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	var p problems
	p.text("note", strings.TrimSpace(in.Note), 1, 2000)
	if err := p.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.runTransition(w, r, body, id, "approve", in.Note)
}

func (s *Server) publishRun(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.runTransition(w, r, nil, id, "publish", "")
}

// runTransition approves or publishes a run in one transaction with its
// audit row.
func (s *Server) runTransition(w http.ResponseWriter, r *http.Request, body []byte, id uuid.UUID, action, note string) {
	run, err := s.models.GetRun(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.mutate(w, r, body, func(ctx context.Context, tx pgx.Tx) (txResult, error) {
		uid := mustPrincipal(ctx).UserID
		before := run.Status
		switch action {
		case "approve":
			if err := s.models.Approve(ctx, tx, id, uid, note); err != nil {
				return txResult{}, err
			}
			now := s.clock.Now().UTC()
			run.Status, run.ApprovedBy, run.ApprovedAt = model.RunApproved, &uid, &now
		case "publish":
			if err := s.models.Publish(ctx, tx, id, uid); err != nil {
				return txResult{}, err
			}
			run.Status = model.RunPublished
		}
		after := map[string]any{"status": run.Status}
		if note != "" {
			after["note"] = note
		}
		if err := s.audit.Record(ctx, tx, auditEntry(r, "model_run", id.String(), action, map[string]any{"status": before}, after)); err != nil {
			return txResult{}, err
		}
		out, err := s.runJSON(ctx, run, false)
		if err != nil {
			return txResult{}, err
		}
		return txResult{Status: http.StatusOK, Body: out}, nil
	})
}

// ---- risk profiles -----------------------------------------------------------------------

type riskProfileJSON struct {
	Key            string            `json:"key"`
	Name           string            `json:"name"`
	Version        int               `json:"version"`
	NeutralWeights map[string]string `json:"neutral_weights"`
}

func profileJSON(p model.RiskProfile) riskProfileJSON {
	w := make(map[string]string, len(p.NeutralWeights))
	for k, v := range p.NeutralWeights {
		w[k] = dec6(v)
	}
	return riskProfileJSON{Key: p.Key, Name: p.Name, Version: p.Version, NeutralWeights: w}
}

type profileInput struct {
	Key            string                     `json:"key"`
	Name           *string                    `json:"name"`
	NeutralWeights map[string]decimal.Decimal `json:"neutral_weights"`
}

// checkNeutralWeights requires known nodes and, for the top level and each
// parent present, weights that sum to exactly 1.
func (s *Server) checkNeutralWeights(ctx context.Context, w map[string]decimal.Decimal, p *problems) error {
	if len(w) == 0 || len(w) > 200 {
		p.add("neutral_weights", "out_of_range")
		return nil
	}
	ents, err := s.catalog.ListEntities(ctx, "")
	if err != nil {
		return err
	}
	parent := map[string]string{}
	for _, e := range ents {
		parent[e.Key] = e.ParentKey
	}
	sums := map[string]decimal.Decimal{}
	for k, v := range w {
		par, ok := parent[k]
		if !ok {
			p.add("neutral_weights."+k, "unknown_node")
			continue
		}
		if v.Sign() < 0 || v.GreaterThan(decimal.NewFromInt(1)) || v.Exponent() < -6 {
			p.add("neutral_weights."+k, "out_of_range")
		}
		sums[par] = sums[par].Add(v)
	}
	for par, sum := range sums {
		if !sum.Equal(decimal.NewFromInt(1)) {
			name := par
			if name == "" {
				name = "top level"
			}
			*p = append(*p, apperr.Detail{Field: "neutral_weights", Reason: "weights_sum", Meta: map[string]any{"group": name, "got": sum.String()}})
		}
	}
	return nil
}

func (s *Server) listRiskProfiles(w http.ResponseWriter, r *http.Request) {
	list, err := s.models.ListProfiles(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]riskProfileJSON, 0, len(list))
	for _, p := range list {
		out = append(out, profileJSON(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) getRiskProfile(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if !keyRE.MatchString(key) {
		s.writeError(w, r, apperr.New(apperr.NotFound, "Not found"))
		return
	}
	p, err := s.models.GetProfile(r.Context(), key)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, profileJSON(p))
}

func (s *Server) createRiskProfile(w http.ResponseWriter, r *http.Request) {
	s.saveRiskProfile(w, r, true)
}
func (s *Server) updateRiskProfile(w http.ResponseWriter, r *http.Request) {
	s.saveRiskProfile(w, r, false)
}

func (s *Server) saveRiskProfile(w http.ResponseWriter, r *http.Request, create bool) {
	body, err := readBody(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var in profileInput
	if err := decodeJSON(r, body, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	key := in.Key
	if !create {
		key = chi.URLParam(r, "key")
		if !keyRE.MatchString(key) {
			s.writeError(w, r, apperr.New(apperr.NotFound, "Not found"))
			return
		}
		if in.Key != "" {
			s.writeError(w, r, apperr.New(apperr.ValidationFailed, "The key cannot change", apperr.Detail{Field: "key", Reason: "immutable"}))
			return
		}
	}
	var p problems
	var current *model.RiskProfile
	if create {
		p.key("key", key)
		if in.Name == nil {
			p.add("name", "required")
		}
		if in.NeutralWeights == nil {
			p.add("neutral_weights", "required")
		}
	} else {
		if in.Name == nil && in.NeutralWeights == nil {
			p.add("body", "empty")
		}
		cur, err := s.models.GetProfile(r.Context(), key)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		current = &cur
	}
	next := model.RiskProfile{Key: key}
	if current != nil {
		next.Name, next.NeutralWeights = current.Name, current.NeutralWeights
	}
	if in.Name != nil {
		p.text("name", *in.Name, 1, 200)
		next.Name = *in.Name
	}
	if in.NeutralWeights != nil {
		next.NeutralWeights = in.NeutralWeights
		if err := s.checkNeutralWeights(r.Context(), in.NeutralWeights, &p); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	if err := p.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	if create {
		if _, err := s.models.GetProfile(r.Context(), key); err == nil {
			s.writeError(w, r, apperr.New(apperr.Conflict, "Risk profile "+key+" already exists"))
			return
		}
	}
	s.mutate(w, r, body, func(ctx context.Context, tx pgx.Tx) (txResult, error) {
		saved, err := s.models.SaveProfile(ctx, tx, next)
		if err != nil {
			return txResult{}, err
		}
		var before any
		action, status := "create", http.StatusCreated
		if current != nil {
			before, action, status = profileJSON(*current), "update", http.StatusOK
		}
		out := profileJSON(saved)
		if err := s.audit.Record(ctx, tx, auditEntry(r, "risk_profile", key, action, before, out)); err != nil {
			return txResult{}, err
		}
		return txResult{Status: status, Body: out}, nil
	})
}
