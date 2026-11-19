package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"cloud.google.com/go/civil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	"github.com/shubham-kukreja/core-jobs-service/internal/model"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
)

var (
	runID   = uuid.MustParse("f04b1c2d-3e4f-4a5b-8c6d-7e8f9a0b1c2d")
	runID2  = uuid.MustParse("a14b1c2d-3e4f-4a5b-8c6d-7e8f9a0b1c2e")
	runAsOf = civil.Date{Year: 2026, Month: 10, Day: 31}
)

func testConfig() model.Config {
	return model.Config{
		Steps: []string{"composite", "veto", "tilt", "normalize", "rollup"}, MissingScore: 50, Normalisation: model.NormProportional,
		Scorecards: map[string]model.Scorecard{
			"l1": {Buckets: []string{"l1.equity", "l1.debt"}, TiltCap: 0.2, ScoreSeries: "{bucket}::{signal}",
				Signals: []model.Signal{{Key: "valuation", Weight: 1}}},
			"equity": {Parent: "l1.equity", Buckets: []string{"equity.large", "equity.mid"}, TiltCap: 0.3, ScoreSeries: "{bucket}::{signal}",
				Signals: []model.Signal{{Key: "valuation", Weight: 1}}},
		},
		Governance: model.Governance{PercentileWindowYears: 15, PercentileMinObservations: 24},
	}
}

func testRun(status model.RunStatus) model.Run {
	job := int64(7)
	return model.Run{ID: 1, UUID: runID, ModelKey: "maaf", ConfigVersion: 1, RiskProfileKey: "balanced", RiskProfileID: 3,
		AsOf: runAsOf, KnowledgeCutoff: testNow, Params: model.Params{SectorsHeld: []string{}}, Status: status, JobRunID: &job,
		Warnings: []model.Warning{{Code: "missing_score", Series: "l1.debt::valuation", Node: "l1.debt", Used: 50}}, CreatedAt: testNow}
}

func f64(v float64) *float64 { return &v }

func testNodes() []model.Node {
	tr, fa := true, false
	return []model.Node{
		{Key: "l1.equity::composite", Num: f64(60), Step: "composite", Formula: "1×60 = 60", Inputs: model.NodeInputs{Datapoints: []int64{41}, Params: map[string]float64{"valuation": 1}}},
		{Key: "l1.equity::veto", Bool: &fa, Step: "veto", Formula: "no trigger active"},
		{Key: "l1.equity::tilt", Num: f64(0.04), Step: "tilt", Formula: "clamp(...)", Inputs: model.NodeInputs{Nodes: []string{"l1.equity::composite", "l1.equity::veto"}, Params: map[string]float64{"cap": 0.2}}},
		{Key: "l1.equity::weight", Num: f64(0.6), Step: "normalize"},
		{Key: "l1.debt::composite", Num: f64(50), Step: "composite"},
		{Key: "l1.debt::veto", Bool: &tr, Step: "veto", IsOverride: true},
		{Key: "l1.debt::weight", Num: f64(0.4), Step: "normalize"},
		{Key: "equity.large::weight", Num: f64(0.5), Step: "normalize"},
		{Key: "input:l1.equity::valuation", Num: f64(60), Step: "input"},
	}
}

func testAlloc() []model.AllocationLine {
	return []model.AllocationLine{
		{LineItem: "l1.debt", Group: "l1.debt", GroupWeight: d6("0.4"), WithinGroup: d6("1"), PortfolioWeight: d6("0.4"), NeutralWeight: d6("0.4")},
		{LineItem: "equity.mid", Group: "l1.equity", GroupWeight: d6("0.6"), WithinGroup: d6("0.5"), PortfolioWeight: d6("0.3"), NeutralWeight: d6("0.3")},
		{LineItem: "equity.large", Group: "l1.equity", GroupWeight: d6("0.6"), WithinGroup: d6("0.5"), PortfolioWeight: d6("0.3"), NeutralWeight: d6("0.3")},
	}
}

func d6(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func modelHarness(t *testing.T) *harness {
	h := newHarness(t)
	cv := model.ConfigVersion{ID: 1, ModelKey: "maaf", Version: 1, Status: model.ConfigActive, Config: testConfig(), CreatedBy: &adminUser.ID, CreatedAt: testNow}
	h.models.ListFunc = func(_ context.Context, m string) ([]model.ConfigVersion, error) {
		if m != "maaf" {
			return nil, nil
		}
		return []model.ConfigVersion{cv}, nil
	}
	h.models.ActiveFunc = func(context.Context, string) (model.ConfigVersion, error) { return cv, nil }
	h.models.GetFunc = func(_ context.Context, _ string, v int) (model.ConfigVersion, error) {
		if v != 1 {
			return model.ConfigVersion{}, apperr.Wrap(model.ErrNotFound, apperr.NotFound, "config not found")
		}
		return cv, nil
	}
	h.models.GetRunFunc = func(_ context.Context, id uuid.UUID) (model.Run, error) {
		if id != runID && id != runID2 {
			return model.Run{}, apperr.Wrap(model.ErrNotFound, apperr.NotFound, "run not found")
		}
		r := testRun(model.RunDraft)
		r.UUID = id
		return r, nil
	}
	h.models.RunValuesFunc = func(_ context.Context, id uuid.UUID, step string) ([]model.Node, error) {
		var out []model.Node
		for _, n := range testNodes() {
			if id == runID2 && n.Key == "l1.equity::composite" {
				n.Num = f64(70)
			}
			if step == "" || n.Step == step {
				out = append(out, n)
			}
		}
		return out, nil
	}
	h.models.AllocationFunc = func(_ context.Context, id uuid.UUID) ([]model.AllocationLine, error) {
		a := testAlloc()
		if id == runID2 {
			a[1].PortfolioWeight, a[2].PortfolioWeight = d6("0.25"), d6("0.35")
		}
		return a, nil
	}
	h.models.ProfileByIDFunc = func(context.Context, int64) (model.RiskProfile, error) {
		return model.RiskProfile{ID: 3, Key: "balanced", Name: "Balanced", Version: 1,
			NeutralWeights: map[string]decimal.Decimal{"l1.equity": d6("0.6"), "l1.debt": d6("0.4")}}, nil
	}
	return h
}

func TestModelConfigEndpoints(t *testing.T) {
	h := modelHarness(t)
	rec := h.do(req{method: "GET", path: "/v1/models/maaf/configs", op: "/v1/models/{model}/configs", token: viewerToken})
	wantStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"created_by":{"id":"0b8f2d4e-1c3a-4f5b-9d6e-7a8b9c0d1e2f","name":"Admin"}`) {
		t.Fatal(rec.Body.String())
	}
	wantCode(t, h.do(req{method: "GET", path: "/v1/models/nope/configs", op: "/v1/models/{model}/configs", token: viewerToken}), 404, "not_found")
	wantStatus(t, h.do(req{method: "GET", path: "/v1/models/maaf/configs/active", op: "/v1/models/{model}/configs/{version}", token: viewerToken}), 200)
	wantStatus(t, h.do(req{method: "GET", path: "/v1/models/maaf/configs/1", op: "/v1/models/{model}/configs/{version}", token: viewerToken}), 200)
	wantCode(t, h.do(req{method: "GET", path: "/v1/models/maaf/configs/9", op: "/v1/models/{model}/configs/{version}", token: viewerToken}), 404, "not_found")

	// Create: the draft is stored even when it does not validate.
	var gotPatch []model.PatchOp
	h.models.CreateFunc = func(_ context.Context, _ pgx.Tx, _ string, base int, patch []model.PatchOp, note string, actor int64) (model.ConfigVersion, error) {
		if base != 1 || note != "wider" || actor != adminUser.ID {
			t.Fatalf("create %d %q %d", base, note, actor)
		}
		gotPatch = patch
		return model.ConfigVersion{ModelKey: "maaf", Version: 2, Status: model.ConfigDraft, Config: testConfig()}, nil
	}
	h.models.ValidateConfigFunc = func(context.Context, model.Config) error {
		return apperr.New(apperr.ValidationFailed, "Invalid model config", apperr.Detail{Path: "/scorecards/l1/signals", Reason: "weights_sum", Meta: map[string]any{"got": 0.95}})
	}
	body := `{"base_version":1,"patch":[{"op":"replace","path":"/scorecards/l1/tilt_cap","value":0.3},{"op":"move","from":"/steps/0","path":"/steps/1"}],"note":"wider"}`
	rec = h.as(adminToken, req{method: "POST", path: "/v1/models/maaf/configs", op: "/v1/models/{model}/configs", body: body})
	wantStatus(t, rec, 201)
	if !strings.Contains(rec.Body.String(), `"version":2,"status":"draft","validation":{"ok":false,"errors":[{"path":"/scorecards/l1/signals","reason":"weights_sum"`) {
		t.Fatal(rec.Body.String())
	}
	if len(gotPatch) != 2 || gotPatch[1].From != "/steps/0" || string(gotPatch[0].Value) != "0.3" {
		t.Fatalf("patch = %+v", gotPatch)
	}
	wantCode(t, h.as(analystToken, req{method: "POST", path: "/v1/models/maaf/configs", op: "/v1/models/{model}/configs", body: body}), 403, "forbidden")
	wantCode(t, h.as(adminToken, req{method: "POST", path: "/v1/models/maaf/configs", op: "/v1/models/{model}/configs",
		body: `{"base_version":1,"patch":[{"op":"replace","path":"scorecards","value":1}]}`}), 400, "validation_failed")

	// Validate and activate.
	rec = h.as(adminToken, req{method: "POST", path: "/v1/models/maaf/configs/1/validate", op: "/v1/models/{model}/configs/{version}/validate"})
	wantStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Fatal(rec.Body.String())
	}
	h.models.ActivateFunc = func(context.Context, pgx.Tx, string, int) error {
		return apperr.New(apperr.ValidationFailed, "Invalid model config", apperr.Detail{Path: "/steps/0", Reason: "unknown_step"})
	}
	wantCode(t, h.as(adminToken, req{method: "POST", path: "/v1/models/maaf/configs/1/activate", op: "/v1/models/{model}/configs/{version}/activate"}), 400, "validation_failed")
	h.models.ActivateFunc = func(context.Context, pgx.Tx, string, int) error { return nil }
	rec = h.as(adminToken, req{method: "POST", path: "/v1/models/maaf/configs/1/activate", op: "/v1/models/{model}/configs/{version}/activate"})
	wantStatus(t, rec, 200)

	h.models.DiffFunc = func(_ context.Context, _ string, from, to int) ([]model.PatchOp, error) {
		return []model.PatchOp{{Op: "replace", Path: "/scorecards/l1/tilt_cap", Value: json.RawMessage(`0.3`)}}, nil
	}
	wantStatus(t, h.do(req{method: "GET", path: "/v1/models/maaf/configs/diff?from=1&to=2", op: "/v1/models/{model}/configs/diff", token: viewerToken}), 200)
	wantCode(t, h.do(req{method: "GET", path: "/v1/models/maaf/configs/diff?from=1", op: "/v1/models/{model}/configs/diff", token: viewerToken}), 400, "validation_failed")

	if len(h.audits) != 2 || h.audits[0].Entity != "model_config" || h.audits[1].Action != "activate" {
		t.Fatalf("audits = %+v", h.audits)
	}
}

func TestNodeOverrideEndpoints(t *testing.T) {
	h := modelHarness(t)
	var got model.NodeOverride
	h.models.CreateOverrideFunc = func(_ context.Context, _ pgx.Tx, _ string, o model.NodeOverride) (model.NodeOverride, error) {
		got = o
		o.ID, o.CreatedAt = runID, testNow
		return o, nil
	}
	rec := h.as(adminToken, req{method: "POST", path: "/v1/models/maaf/overrides", op: "/v1/models/{model}/overrides",
		body: `{"node_key":"equity.intl::veto","as_of":"2026-10-31","value":true,"reason":"INR stress episode","valid_until":"2027-01-31"}`})
	wantStatus(t, rec, 201)
	if got.Value.Bool == nil || !*got.Value.Bool || got.ValidUntil == nil || got.ActorID != adminUser.ID || !strings.Contains(rec.Body.String(), `"actor":{"id"`) {
		t.Fatalf("override = %+v, %s", got, rec.Body.String())
	}
	rec = h.as(adminToken, req{method: "POST", path: "/v1/models/maaf/overrides", op: "/v1/models/{model}/overrides",
		body: `{"node_key":"l1.debt::composite","as_of":"2026-10-31","value":72.5,"reason":"x"}`})
	wantStatus(t, rec, 201)
	if got.Value.Num == nil || *got.Value.Num != 72.5 {
		t.Fatalf("numeric override = %+v", got)
	}
	for _, bad := range []string{
		`{"node_key":"x","as_of":"2026-10-31","value":"high","reason":"x"}`,
		`{"node_key":"Bad Key","as_of":"2026-10-31","value":1,"reason":"x"}`,
		`{"node_key":"x","as_of":"2026-10-31","value":1,"reason":"x","valid_until":"2026-01-01"}`,
		`{"node_key":"x","as_of":"2026-10-31","value":1,"reason":"  "}`,
	} {
		wantCode(t, h.as(adminToken, req{method: "POST", path: "/v1/models/maaf/overrides", op: "/v1/models/{model}/overrides", body: bad}), 400, "validation_failed")
	}
	h.models.DeleteOverrideFunc = func(_ context.Context, _ pgx.Tx, id uuid.UUID, _ int64) error {
		if id != runID {
			return apperr.Wrap(model.ErrNotFound, apperr.NotFound, "override not found")
		}
		return nil
	}
	wantStatus(t, h.as(adminToken, req{method: "DELETE", path: "/v1/models/maaf/overrides/" + runID.String(), op: "/v1/models/{model}/overrides/{id}"}), 204)
	wantCode(t, h.as(adminToken, req{method: "DELETE", path: "/v1/models/maaf/overrides/" + runID2.String(), op: "/v1/models/{model}/overrides/{id}"}), 404, "not_found")
	wantCode(t, h.as(analystToken, req{method: "DELETE", path: "/v1/models/maaf/overrides/" + runID.String(), op: "/v1/models/{model}/overrides/{id}"}), 403, "forbidden")
}

func TestRunEndpoints(t *testing.T) {
	h := modelHarness(t)
	h.models.ListRunsFunc = func(_ context.Context, f model.RunFilter) (paging.Page[model.Run], error) {
		if f.Model != "maaf" || f.Status != model.RunDraft || f.From == nil || f.Page.Limit != 1 {
			t.Fatalf("filter = %+v", f)
		}
		return paging.Page[model.Run]{Items: []model.Run{testRun(model.RunDraft)}, Next: "1"}, nil
	}
	rec := h.do(req{method: "GET", path: "/v1/runs?model=maaf&status=draft&from=2026-01-01&limit=1", op: "/v1/runs", token: viewerToken})
	wantStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"job_run_id":"00000000-0000-4000-8000-000000000007"`) || strings.Contains(rec.Body.String(), `"next_cursor":null`) {
		t.Fatal(rec.Body.String())
	}
	wantCode(t, h.do(req{method: "GET", path: "/v1/runs?status=done", op: "/v1/runs", token: viewerToken}), 400, "validation_failed")

	rec = h.do(req{method: "GET", path: "/v1/runs/" + runID.String(), op: "/v1/runs/{id}", token: viewerToken})
	wantStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `{"bucket":"l1.equity","composite":60,"veto":false,"tilt":0.04,"final":0.6,"neutral":0.6}`) {
		t.Fatal(rec.Body.String())
	}
	wantCode(t, h.do(req{method: "GET", path: "/v1/runs/" + uuid.NewString(), op: "/v1/runs/{id}", token: viewerToken}), 404, "not_found")

	rec = h.do(req{method: "GET", path: "/v1/runs/" + runID.String() + "/values?scorecard=l1", op: "/v1/runs/{id}/values", token: viewerToken})
	wantStatus(t, rec, 200)
	if strings.Contains(rec.Body.String(), "input:") || strings.Contains(rec.Body.String(), "equity.large") || !strings.Contains(rec.Body.String(), `"node":"l1.debt::veto","value":true,"step":"veto","is_override":true`) {
		t.Fatal(rec.Body.String())
	}
	wantCode(t, h.do(req{method: "GET", path: "/v1/runs/" + runID.String() + "/values?scorecard=nope", op: "/v1/runs/{id}/values", token: viewerToken}), 400, "validation_failed")
	wantCode(t, h.do(req{method: "GET", path: "/v1/runs/" + runID.String() + "/values?step=input", op: "/v1/runs/{id}/values", token: viewerToken}), 400, "validation_failed")

	h.reader.GetFunc = func(_ context.Context, id int64) (datapoints.Datapoint, error) {
		return datapoints.Datapoint{Resolved: datapoints.Resolved{ID: id, SeriesKey: "l1.equity::valuation", Value: num(60)}}, nil
	}
	rec = h.do(req{method: "GET", path: "/v1/runs/" + runID.String() + "/values/l1.equity::tilt/lineage", op: "/v1/runs/{id}/values/{node_key}/lineage", token: viewerToken})
	wantStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"nodes":{"l1.equity::composite":60,"l1.equity::veto":false}`) {
		t.Fatal(rec.Body.String())
	}
	rec = h.do(req{method: "GET", path: "/v1/runs/" + runID.String() + "/values/l1.equity::composite/lineage", op: "/v1/runs/{id}/values/{node_key}/lineage", token: viewerToken})
	if !strings.Contains(rec.Body.String(), `"datapoints":[{"id":41,"series":"l1.equity::valuation","value":60}]`) {
		t.Fatal(rec.Body.String())
	}
	wantCode(t, h.do(req{method: "GET", path: "/v1/runs/" + runID.String() + "/values/nope::x/lineage", op: "/v1/runs/{id}/values/{node_key}/lineage", token: viewerToken}), 404, "not_found")

	// Allocations: rollup order, 6-dp strings, exact total.
	rec = h.do(req{method: "GET", path: "/v1/runs/" + runID.String() + "/allocations?profile=balanced", op: "/v1/runs/{id}/allocations", token: viewerToken})
	wantStatus(t, rec, 200)
	b := rec.Body.String()
	large, mid, debt := strings.Index(b, "equity.large"), strings.Index(b, "equity.mid"), strings.Index(b, `"line_item":"l1.debt"`)
	if !strings.Contains(b, `"total":"1.000000"`) || large > mid || mid > debt {
		t.Fatal(b)
	}
	wantCode(t, h.do(req{method: "GET", path: "/v1/runs/" + runID.String() + "/allocations?profile=aggressive", op: "/v1/runs/{id}/allocations", token: viewerToken}), 404, "not_found")
	h.models.CurrentRunFunc = func(_ context.Context, m, p string) (model.Run, error) {
		if m != "maaf" || p != "balanced" {
			return model.Run{}, apperr.Wrap(model.ErrNotFound, apperr.NotFound, "none")
		}
		return testRun(model.RunPublished), nil
	}
	wantStatus(t, h.do(req{method: "GET", path: "/v1/allocations/current?model=maaf&profile=balanced", op: "/v1/allocations/current", token: viewerToken}), 200)
	wantCode(t, h.do(req{method: "GET", path: "/v1/allocations/current?model=maaf&profile=x", op: "/v1/allocations/current", token: viewerToken}), 404, "not_found")
	wantCode(t, h.do(req{method: "GET", path: "/v1/allocations/current?model=maaf", op: "/v1/allocations/current", token: viewerToken}), 400, "validation_failed")

	rec = h.do(req{method: "GET", path: "/v1/runs/compare?a=" + runID.String() + "&b=" + runID2.String(), op: "/v1/runs/compare", token: viewerToken})
	wantStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `{"node":"equity.large","from":0.3,"to":0.35,"delta":0.05}`) || !strings.Contains(rec.Body.String(), `{"node":"l1.equity::composite","a":60,"b":70}`) ||
		strings.Contains(rec.Body.String(), `"node":"l1.debt"`) {
		t.Fatal(rec.Body.String())
	}

	// Approve (analyst, note required) and publish (admin).
	var approved, published bool
	h.models.ApproveFunc = func(_ context.Context, _ pgx.Tx, _ uuid.UUID, actor int64, note string) error {
		approved = actor == analystUser.ID && note == "Reviewed"
		return nil
	}
	h.models.PublishFunc = func(context.Context, pgx.Tx, uuid.UUID, int64) error { published = true; return nil }
	wantCode(t, h.as(analystToken, req{method: "POST", path: "/v1/runs/" + runID.String() + "/approve", op: "/v1/runs/{id}/approve", body: `{"note":""}`}), 400, "validation_failed")
	rec = h.as(analystToken, req{method: "POST", path: "/v1/runs/" + runID.String() + "/approve", op: "/v1/runs/{id}/approve", body: `{"note":"Reviewed"}`})
	wantStatus(t, rec, 200)
	if !approved || !strings.Contains(rec.Body.String(), `"status":"approved","warnings"`) || !strings.Contains(rec.Body.String(), `"approved_by":{"id":"1c9f3e5f`) {
		t.Fatal(rec.Body.String())
	}
	wantCode(t, h.as(analystToken, req{method: "POST", path: "/v1/runs/" + runID.String() + "/publish", op: "/v1/runs/{id}/publish"}), 403, "forbidden")
	rec = h.as(adminToken, req{method: "POST", path: "/v1/runs/" + runID.String() + "/publish", op: "/v1/runs/{id}/publish"})
	wantStatus(t, rec, 200)
	if !published || !strings.Contains(rec.Body.String(), `"status":"published"`) {
		t.Fatal(rec.Body.String())
	}
	h.models.PublishFunc = func(context.Context, pgx.Tx, uuid.UUID, int64) error {
		return apperr.Wrap(model.ErrInvalidState, apperr.Conflict, "run is superseded")
	}
	wantCode(t, h.as(adminToken, req{method: "POST", path: "/v1/runs/" + runID.String() + "/publish", op: "/v1/runs/{id}/publish"}), 409, "conflict")
	if len(h.audits) != 2 || h.audits[0].Entity != "model_run" || h.audits[0].Action != "approve" {
		t.Fatalf("audits = %+v", h.audits)
	}
}

func TestRiskProfileEndpoints(t *testing.T) {
	h := modelHarness(t)
	h.cat.ListEntitiesFunc = func(context.Context, string) ([]catalog.Entity, error) {
		return []catalog.Entity{{Key: "l1.equity"}, {Key: "l1.debt"}, {Key: "equity.large", ParentKey: "l1.equity"}, {Key: "equity.mid", ParentKey: "l1.equity"}}, nil
	}
	saved := map[string]model.RiskProfile{}
	h.models.GetProfileFunc = func(_ context.Context, k string) (model.RiskProfile, error) {
		if p, ok := saved[k]; ok {
			return p, nil
		}
		return model.RiskProfile{}, apperr.Wrap(model.ErrNotFound, apperr.NotFound, "risk profile not found")
	}
	h.models.SaveProfileFunc = func(_ context.Context, _ pgx.Tx, p model.RiskProfile) (model.RiskProfile, error) {
		p.Version = saved[p.Key].Version + 1
		saved[p.Key] = p
		return p, nil
	}
	h.models.ListProfilesFunc = func(context.Context) ([]model.RiskProfile, error) { return []model.RiskProfile{saved["balanced"]}, nil }

	body := `{"key":"balanced","name":"Balanced","neutral_weights":{"l1.equity":0.55,"l1.debt":0.45,"equity.large":0.7,"equity.mid":0.3}}`
	rec := h.as(adminToken, req{method: "POST", path: "/v1/risk-profiles", op: "/v1/risk-profiles", body: body})
	wantStatus(t, rec, 201)
	if !strings.Contains(rec.Body.String(), `"l1.equity":"0.550000"`) || !saved["balanced"].NeutralWeights["l1.debt"].Equal(d6("0.45")) {
		t.Fatal(rec.Body.String())
	}
	wantCode(t, h.as(adminToken, req{method: "POST", path: "/v1/risk-profiles", op: "/v1/risk-profiles", body: body}), 409, "conflict")
	rec = h.as(adminToken, req{method: "POST", path: "/v1/risk-profiles", op: "/v1/risk-profiles",
		body: `{"key":"aggr","name":"Aggressive","neutral_weights":{"l1.equity":0.6,"l1.debt":0.3,"l1.gold":0.1}}`})
	wantCode(t, rec, 400, "validation_failed")
	if !strings.Contains(rec.Body.String(), `"group":"top level"`) || !strings.Contains(rec.Body.String(), `"neutral_weights.l1.gold"`) {
		t.Fatal(rec.Body.String())
	}

	wantStatus(t, h.do(req{method: "GET", path: "/v1/risk-profiles", op: "/v1/risk-profiles", token: viewerToken}), 200)
	wantStatus(t, h.do(req{method: "GET", path: "/v1/risk-profiles/balanced", op: "/v1/risk-profiles/{key}", token: viewerToken}), 200)
	wantCode(t, h.do(req{method: "GET", path: "/v1/risk-profiles/nope", op: "/v1/risk-profiles/{key}", token: viewerToken}), 404, "not_found")

	rec = h.as(adminToken, req{method: "PATCH", path: "/v1/risk-profiles/balanced", op: "/v1/risk-profiles/{key}", body: `{"name":"Balanced (2027)"}`})
	wantStatus(t, rec, 200)
	if saved["balanced"].Version != 2 || !saved["balanced"].NeutralWeights["l1.equity"].Equal(d6("0.55")) {
		t.Fatalf("patched = %+v", saved["balanced"])
	}
	wantCode(t, h.as(adminToken, req{method: "PATCH", path: "/v1/risk-profiles/balanced", op: "/v1/risk-profiles/{key}", body: `{}`}), 400, "validation_failed")
	wantCode(t, h.as(adminToken, req{method: "PATCH", path: "/v1/risk-profiles/nope", op: "/v1/risk-profiles/{key}", body: `{"name":"x"}`}), 404, "not_found")
	wantCode(t, h.as(analystToken, req{method: "PATCH", path: "/v1/risk-profiles/balanced", op: "/v1/risk-profiles/{key}", body: `{"name":"x"}`}), 403, "forbidden")
}

func TestModelRunPublishNeedsAdmin(t *testing.T) {
	h := modelHarness(t)
	model.RegisterRun(h.kinds, model.JobDeps{})
	h.enq.EnqueueTxFunc = func(_ context.Context, _ pgx.Tx, _ jobs.Args, _ ...jobs.Opt) (jobs.RunRef, error) {
		return jobs.RunRef{ID: 9, UUID: uuidOf(9), Status: jobs.StatusQueued}, nil
	}
	body := `{"kind":"model.run","args":{"model":"maaf","as_of":"2026-12-31","params":{},"publish":true}}`
	wantCode(t, h.as(analystToken, req{method: "POST", path: "/v1/jobs", op: "/v1/jobs", body: body}), 403, "forbidden")
	wantStatus(t, h.as(adminToken, req{method: "POST", path: "/v1/jobs", op: "/v1/jobs", body: body}), 202)
	wantStatus(t, h.as(analystToken, req{method: "POST", path: "/v1/jobs", op: "/v1/jobs",
		body: `{"kind":"model.run","args":{"model":"maaf","as_of":"2026-12-31","params":{"sectors_held":["sector.capgoods"]},"publish":false}}`}), 202)
}
