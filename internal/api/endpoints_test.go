package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/civil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/ingest"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
)

func testSeries() catalog.Series {
	return catalog.Series{
		Key: "pe.nifty100", Name: "Nifty 100 P/E", EntityKey: "idx.nifty100", Layer: catalog.LayerRaw,
		ValueType: catalog.ValueNum, Unit: "x", Frequency: catalog.FreqMonthly, Align: catalog.AlignLast,
		SourcePriority: []string{"override", "analyst"}, Active: true, CreatedAt: testNow, UpdatedAt: testNow,
		Registry: catalog.RegistryMeta{Group: "equity", StaleAfter: 40 * 24 * time.Hour},
	}
}

func derivedSeries() catalog.Series {
	s := testSeries()
	s.Key, s.Layer = "pctile.180m.spread.mid_large", catalog.LayerPercentile
	s.Derivation = &catalog.Derivation{Fn: "percentile_rank", Inputs: map[string]string{"x": "spread.mid_large"}, Params: json.RawMessage(`{"window_months":180}`), Version: 2}
	return s
}

func snapshotEmpty(context.Context, []string, civil.Date, datapoints.ReadOpts) (map[string]datapoints.Resolved, error) {
	return map[string]datapoints.Resolved{}, nil
}

func seriesLookup(known ...catalog.Series) func(context.Context, string) (catalog.Series, error) {
	return func(_ context.Context, k string) (catalog.Series, error) {
		for _, s := range known {
			if s.Key == k {
				return s, nil
			}
		}
		return catalog.Series{}, catalog.ErrNotFound
	}
}

func num(f float64) datapoints.Value { return datapoints.Num(f) }

// ---------------------------------------------------------------- catalog

func TestEntities(t *testing.T) {
	h := newHarness(t)
	h.cat.ListEntitiesFunc = func(_ context.Context, kind string) ([]catalog.Entity, error) {
		return []catalog.Entity{{Key: "sector.it", Kind: "sector", Name: "IT", ParentKey: "l1.equity", Active: true}}, nil
	}
	wantStatus(t, h.do(req{method: "GET", path: "/v1/entities?kind=sector", op: "/v1/entities", token: viewerToken}), 200)
	wantCode(t, h.do(req{method: "GET", path: "/v1/entities?kind=planet", op: "/v1/entities", token: viewerToken}), 400, "validation_failed")

	h.cat.EntityFunc = func(_ context.Context, k string) (catalog.Entity, error) {
		if k == "sector.it" {
			return catalog.Entity{Key: k, Kind: "sector", Name: "IT", Active: true}, nil
		}
		return catalog.Entity{}, catalog.ErrNotFound
	}
	h.cat.SaveEntityFunc = func(_ context.Context, _ pgx.Tx, e catalog.Entity) (catalog.Entity, error) { return e, nil }
	rec := h.as(adminToken, req{method: "POST", path: "/v1/entities", op: "/v1/entities", body: `{"key":"sector.capgoods","kind":"sector","name":"Capital Goods","parent":"l1.equity"}`})
	wantStatus(t, rec, 201)
	wantCode(t, h.as(adminToken, req{method: "POST", path: "/v1/entities", op: "/v1/entities", body: `{"key":"sector.it","kind":"sector","name":"IT"}`}), 409, "conflict")

	rec = h.as(adminToken, req{method: "PATCH", path: "/v1/entities/sector.it", op: "/v1/entities/{key}", body: `{"name":"IT Services","parent":null}`})
	wantStatus(t, rec, 200)
	wantCode(t, h.as(adminToken, req{method: "PATCH", path: "/v1/entities/sector.zz", op: "/v1/entities/{key}", body: `{"name":"Z"}`}), 404, "not_found")
	wantCode(t, h.as(adminToken, req{method: "PATCH", path: "/v1/entities/sector.it", op: "/v1/entities/{key}", body: `{}`}), 400, "validation_failed")

	if n := len(h.audits); n != 2 || h.audits[0].Action != "create" || h.audits[1].Action != "update" || h.audits[1].Before == nil {
		t.Fatalf("audits: %+v", h.audits)
	}
	if *h.audits[0].ActorID != adminUser.ID {
		t.Fatal("audit actor")
	}
}

func TestSourcesOmitConfig(t *testing.T) {
	h := newHarness(t, func(_ *Config, d *Deps) { d.Connectors = connectors{"fred": true} })
	h.cat.ListSourcesFunc = func(context.Context) ([]catalog.Source, error) {
		return []catalog.Source{{Key: "fred", Kind: catalog.SourceExternalAPI, Name: "FRED", Active: true}, {Key: "amfi", Kind: catalog.SourceExternalAPI, Name: "AMFI", Active: true}}, nil
	}
	rec := h.do(req{method: "GET", path: "/v1/sources", op: "/v1/sources", token: viewerToken})
	wantStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"key":"fred","kind":"external_api","name":"FRED","active":true,"connector_available":true`) ||
		strings.Contains(rec.Body.String(), "credential") {
		t.Fatal(rec.Body.String())
	}
}

type connectors map[string]bool

func (c connectors) Get(k string) (ingest.Connector, bool) { return nil, c[k] }

func TestSeriesReadAndDeps(t *testing.T) {
	h := newHarness(t)
	h.cat.SeriesFunc = seriesLookup(testSeries(), derivedSeries())
	wantStatus(t, h.do(req{method: "GET", path: "/v1/series/pe.nifty100", op: "/v1/series/{key}", token: viewerToken}), 200)
	rec := h.do(req{method: "GET", path: "/v1/series/pctile.180m.spread.mid_large", op: "/v1/series/{key}", token: viewerToken})
	wantStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"derivation_ver":2`) {
		t.Fatal(rec.Body.String())
	}
	wantCode(t, h.do(req{method: "GET", path: "/v1/series/nope", op: "/v1/series/{key}", token: viewerToken}), 404, "not_found")

	h.cat.DepsFunc = func(_ context.Context, k string, d catalog.DepDirection, depth int) ([]catalog.DepEdge, error) {
		if d != catalog.DepsDown || depth != 2 {
			t.Fatalf("deps args %s %d", d, depth)
		}
		return []catalog.DepEdge{{SeriesKey: "spread.mid_large", DependsOnKey: k, Depth: 1}}, nil
	}
	wantStatus(t, h.do(req{method: "GET", path: "/v1/series/pe.nifty100/deps?direction=down&depth=2", op: "/v1/series/{key}/deps", token: viewerToken}), 200)
	wantCode(t, h.do(req{method: "GET", path: "/v1/series/pe.nifty100/deps?depth=99", op: "/v1/series/{key}/deps", token: viewerToken}), 400, "validation_failed")
	wantCode(t, h.do(req{method: "GET", path: "/v1/series?layer=bogus", op: "/v1/series", token: viewerToken}), 400, "validation_failed")
}

func TestCreateSeriesEnqueuesRebuild(t *testing.T) {
	h := newHarness(t)
	h.cat.SeriesFunc = seriesLookup(testSeries())
	var saved catalog.SeriesInput
	h.cat.SaveSeriesFunc = func(_ context.Context, _ pgx.Tx, in catalog.SeriesInput) (catalog.SaveResult, error) {
		saved = in
		s := derivedSeries()
		s.Derivation.Version = 1
		return catalog.SaveResult{Series: s, Rebuild: true}, nil
	}
	body := `{"key":"pctile.180m.spread.mid_large","name":"Mid–Large spread pctile","entity":"equity.mid","layer":"percentile",
	"value_type":"num","unit":"pctile","frequency":"monthly","validation":{"min":0,"max":100},"min_history":24,
	"derivation":{"fn":"percentile_rank","inputs":{"x":"spread.mid_large"},"params":{"window_months":180}},"tags":["data_trackers"],
	"registry":{"group":"equity","stale_after":"40d"}}`
	rec := h.as(adminToken, req{method: "POST", path: "/v1/series", op: "/v1/series", body: body})
	wantStatus(t, rec, 201)
	if len(h.rebuild.calls) != 1 || !strings.Contains(rec.Body.String(), `"rebuild_job":{"id":"a1c2e3f4-5b6c-4d7e-8f90-a1b2c3d4e5f6","kind":"derive.cascade"`) {
		t.Fatalf("rebuild: %v %s", h.rebuild.calls, rec.Body.String())
	}
	if saved.Registry.StaleAfter != 40*24*time.Hour || saved.Derivation.Fn != "percentile_rank" {
		t.Fatalf("input mapping: %+v", saved)
	}
	wantCode(t, h.as(adminToken, req{method: "POST", path: "/v1/series", op: "/v1/series",
		body: `{"key":"pe.nifty100","name":"dup","layer":"raw","value_type":"num","frequency":"monthly"}`}), 409, "conflict")

	h.cat.SaveSeriesFunc = func(context.Context, pgx.Tx, catalog.SeriesInput) (catalog.SaveResult, error) {
		return catalog.SaveResult{}, catalog.ErrCycle
	}
	h.cat.SetSeriesActiveFunc = func(context.Context, pgx.Tx, string, bool) error { return nil }
	wantCode(t, h.as(adminToken, req{method: "PATCH", path: "/v1/series/pe.nifty100", op: "/v1/series/{key}",
		body: `{"derivation":{"fn":"expr","inputs":{"a":"pe.nifty100"},"params":{"expr":"a"}}}`}), 409, "conflict")
	if h.db.rollbacks == 0 {
		t.Fatal("failed save must roll back")
	}
}

// ---------------------------------------------------------------- datapoints

func writerOK(h *harness, got *[]datapoints.Point, meta *datapoints.WriteMeta) {
	h.writer.WriteFunc = func(_ context.Context, _ pgx.Tx, m datapoints.WriteMeta, pts []datapoints.Point) (datapoints.WriteResult, error) {
		*got, *meta = pts, m
		return datapoints.WriteResult{BatchID: uuid.MustParse("5b0e8c1a-2d3f-4e5a-8b6c-7d8e9f0a1b2c"), Written: len(pts)}, nil
	}
}

const writeBody = `{"source":"analyst","points":[
 {"series":"judg.rbi.stance","obs_date":"2026-10-31","value":"cutting","note":"Oct MPC"},
 {"series":"breadth.nifty100.up","obs_date":"2026-10-31","value":48},
 {"series":"trig.currency_stress","obs_date":"2026-10-31","value":false}]}`

func TestWriteDatapoints(t *testing.T) {
	h := newHarness(t)
	var pts []datapoints.Point
	var meta datapoints.WriteMeta
	writerOK(h, &pts, &meta)
	rec := h.as(analystToken, req{method: "POST", path: "/v1/datapoints", op: "/v1/datapoints", body: writeBody})
	wantStatus(t, rec, 201)
	if len(pts) != 3 || *pts[0].Value.Cat != "cutting" || *pts[1].Value.Num != 48 || *pts[2].Value.Bool {
		t.Fatalf("points: %+v", pts)
	}
	if meta.Source != "analyst" || *meta.ActorID != analystUser.ID || meta.Origin != datapoints.OriginPrimary {
		t.Fatalf("meta: %+v", meta)
	}
	if h.db.commits != 1 || len(h.audits) != 1 || h.audits[0].Entity != "datapoint_batch" {
		t.Fatalf("tx/audit: %d %+v", h.db.commits, h.audits)
	}
}

func TestOverrideRequiresAdmin(t *testing.T) {
	h := newHarness(t)
	var pts []datapoints.Point
	var meta datapoints.WriteMeta
	writerOK(h, &pts, &meta)
	body := `{"source":"override","points":[{"series":"equity.large::relvalue","obs_date":"2026-10-31","value":60}]}`
	wantCode(t, h.as(analystToken, req{method: "POST", path: "/v1/datapoints", op: "/v1/datapoints", body: body}), 403, "forbidden")
	if pts != nil {
		t.Fatal("analyst override reached the writer")
	}
	wantStatus(t, h.as(adminToken, req{method: "POST", path: "/v1/datapoints", op: "/v1/datapoints", body: body}), 201)
	if meta.Source != "override" {
		t.Fatal(meta.Source)
	}
	wantCode(t, h.as(adminToken, req{method: "POST", path: "/v1/datapoints", op: "/v1/datapoints",
		body: `{"source":"computed","points":[{"series":"a","obs_date":"2026-10-31","value":1}]}`}), 400, "validation_failed")
}

func TestWriteDatapointsValidation(t *testing.T) {
	h := newHarness(t)
	for name, body := range map[string]string{
		"null value": `{"source":"analyst","points":[{"series":"a","obs_date":"2026-10-31","value":null}]}`,
		"bad date":   `{"source":"analyst","points":[{"series":"a","obs_date":"2026-02-30","value":1}]}`,
		"bad key":    `{"source":"analyst","points":[{"series":"A B","obs_date":"2026-10-31","value":1}]}`,
		"no points":  `{"source":"analyst","points":[]}`,
		"object":     `{"source":"analyst","points":[{"series":"a","obs_date":"2026-10-31","value":{"x":1}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			wantCode(t, h.as(analystToken, req{method: "POST", path: "/v1/datapoints", op: "/v1/datapoints", body: body}), 400, "validation_failed")
		})
	}

	h.writer.WriteFunc = func(_ context.Context, _ pgx.Tx, _ datapoints.WriteMeta, pts []datapoints.Point) (datapoints.WriteResult, error) {
		return datapoints.WriteResult{BatchID: uuid.New(), Rejected: []datapoints.Rejection{{Index: 0, SeriesKey: "a", Reason: "unknown_series"}}}, nil
	}
	rec := h.as(analystToken, req{method: "POST", path: "/v1/datapoints", op: "/v1/datapoints",
		body: `{"source":"analyst","points":[{"series":"a","obs_date":"2026-10-31","value":1}]}`})
	wantCode(t, rec, 400, "validation_failed")
	if h.db.commits != 0 || len(h.audits) != 0 {
		t.Fatal("all-rejected batch must not commit or audit")
	}
}

func TestIdempotency(t *testing.T) {
	h := newHarness(t)
	calls := 0
	h.writer.WriteFunc = func(_ context.Context, _ pgx.Tx, _ datapoints.WriteMeta, pts []datapoints.Point) (datapoints.WriteResult, error) {
		calls++
		return datapoints.WriteResult{BatchID: uuid.New(), Written: len(pts)}, nil
	}
	key := map[string]string{"Idempotency-Key": "retry-7f3a"}
	first := h.as(analystToken, req{method: "POST", path: "/v1/datapoints", op: "/v1/datapoints", body: writeBody, headers: key})
	wantStatus(t, first, 201)
	replay := h.as(analystToken, req{method: "POST", path: "/v1/datapoints", op: "/v1/datapoints", body: writeBody, headers: key})
	wantStatus(t, replay, 201)
	if calls != 1 || strings.TrimSpace(replay.Body.String()) != strings.TrimSpace(first.Body.String()) || replay.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: calls=%d\n%s\n%s", calls, first.Body, replay.Body)
	}
	other := strings.Replace(writeBody, "48", "49", 1)
	wantCode(t, h.as(analystToken, req{method: "POST", path: "/v1/datapoints", op: "/v1/datapoints", body: other, headers: key}), 422, "idempotency_mismatch")
	wantCode(t, h.as(analystToken, req{method: "POST", path: "/v1/datapoints", op: "/v1/datapoints", body: writeBody,
		headers: map[string]string{"Idempotency-Key": "bad key!"}}), 400, "validation_failed")
	// Keys are per user: another user's identical key is independent.
	h.clock.Advance(25 * time.Hour)
	wantStatus(t, h.as(analystToken, req{method: "POST", path: "/v1/datapoints", op: "/v1/datapoints", body: other,
		headers: map[string]string{"Idempotency-Key": "fresh-key"}}), 201)
}

func TestSeriesDatapointsPointInTime(t *testing.T) {
	h := newHarness(t)
	h.cat.SeriesFunc = seriesLookup(testSeries())
	jan, feb := civil.Date{Year: 2026, Month: 1, Day: 31}, civil.Date{Year: 2026, Month: 2, Day: 28}
	cut := time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)
	h.reader.RangeFunc = func(_ context.Context, _ string, _, _ civil.Date, o datapoints.ReadOpts) ([]datapoints.Resolved, error) {
		rows := []datapoints.Resolved{{ID: 9012, SeriesKey: "pe.nifty100", ObsDate: jan, Value: num(71.4), Source: "computed", KnownAt: cut.Add(-240 * time.Hour), DerivVer: 1}}
		if o.Cutoff.After(cut) {
			rows = append(rows, datapoints.Resolved{ID: 9155, SeriesKey: "pe.nifty100", ObsDate: feb, Value: num(70), Source: "computed", KnownAt: cut.Add(24 * time.Hour)})
		}
		return rows, nil
	}
	rec := h.do(req{method: "GET", path: "/v1/series/pe.nifty100/datapoints?from=2026-01-01&to=2026-03-31&cutoff=2026-02-15T00:00:00Z",
		op: "/v1/series/{key}/datapoints", token: viewerToken})
	wantStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"obs_date":"2026-02-28","value":null,"source":null,"known_at":null,"derivation_ver":null,"reason":"not_known_at_cutoff"`) {
		t.Fatal(rec.Body.String())
	}
	wantCode(t, h.do(req{method: "GET", path: "/v1/series/pe.nifty100/datapoints?from=2026-03-01&to=2026-01-01",
		op: "/v1/series/{key}/datapoints", token: viewerToken}), 400, "validation_failed")
	wantCode(t, h.do(req{method: "GET", path: "/v1/series/pe.nifty100/datapoints?from=2026-01-01&to=2026-03-01&cutoff=yesterday",
		op: "/v1/series/{key}/datapoints", token: viewerToken}), 400, "validation_failed")
	wantCode(t, h.do(req{method: "GET", path: "/v1/series/nope/datapoints?from=2026-01-01&to=2026-03-01",
		op: "/v1/series/{key}/datapoints", token: viewerToken}), 404, "not_found")

	h.reader.AtFunc = func(_ context.Context, _ string, d civil.Date, _ datapoints.ReadOpts) (*datapoints.Resolved, error) {
		if d == jan {
			return &datapoints.Resolved{ID: 9012, SeriesKey: "pe.nifty100", ObsDate: jan, Value: num(71.4), Source: "computed", KnownAt: cut}, nil
		}
		return nil, nil
	}
	wantStatus(t, h.do(req{method: "GET", path: "/v1/series/pe.nifty100/datapoints/2026-01-31", op: "/v1/series/{key}/datapoints/{obs_date}", token: viewerToken}), 200)
	rec = h.do(req{method: "GET", path: "/v1/series/pe.nifty100/datapoints/2026-04-30", op: "/v1/series/{key}/datapoints/{obs_date}", token: viewerToken})
	if !strings.Contains(rec.Body.String(), `"reason":"no_value"`) {
		t.Fatal(rec.Body.String())
	}
}

func TestVersionsLineageRetract(t *testing.T) {
	h := newHarness(t)
	h.cat.SeriesFunc = seriesLookup(testSeries(), derivedSeries())
	d := civil.Date{Year: 2026, Month: 10, Day: 31}
	uid := analystUser.ID
	h.reader.VersionsFunc = func(context.Context, string, civil.Date) ([]datapoints.Datapoint, error) {
		return []datapoints.Datapoint{
			{Resolved: datapoints.Resolved{ID: 30122, Value: num(6.72), Source: "rbi", KnownAt: testNow.Add(-time.Hour)}, Status: "active"},
			{Resolved: datapoints.Resolved{ID: 30411, Value: num(6.70), Source: "override", KnownAt: testNow}, Status: "active", ActorID: &uid, Note: "revised"},
		}, nil
	}
	h.reader.AtFunc = func(context.Context, string, civil.Date, datapoints.ReadOpts) (*datapoints.Resolved, error) {
		return &datapoints.Resolved{ID: 30411, ObsDate: d}, nil
	}
	rec := h.do(req{method: "GET", path: "/v1/series/pe.nifty100/datapoints/2026-10-31/versions", op: "/v1/series/{key}/datapoints/{obs_date}/versions", token: viewerToken})
	wantStatus(t, rec, 200)
	body := rec.Body.String()
	if !strings.Contains(body, `"resolved_id":30411`) || !strings.Contains(body, analystUser.Email) || strings.Index(body, "30411") > strings.Index(body, "30122") {
		t.Fatal(body)
	}

	h.reader.LineageFunc = func(context.Context, int64, int) (datapoints.Lineage, error) {
		return datapoints.Lineage{
			Datapoint: datapoints.Resolved{ID: 41877, SeriesKey: "pctile.180m.spread.mid_large", ObsDate: d, Value: num(38)},
			Inputs:    []datapoints.Lineage{{Datapoint: datapoints.Resolved{ID: 41790, SeriesKey: "spread.mid_large", ObsDate: d, Value: num(-2.2)}}},
		}, nil
	}
	rec = h.do(req{method: "GET", path: "/v1/datapoints/41877/lineage?depth=3", op: "/v1/datapoints/{id}/lineage", token: viewerToken})
	wantStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"derivation":{"fn":"percentile_rank","version":2}`) {
		t.Fatal(rec.Body.String())
	}
	wantCode(t, h.do(req{method: "GET", path: "/v1/datapoints/abc/lineage", op: "/v1/datapoints/{id}/lineage", token: viewerToken}), 400, "validation_failed")

	h.reader.GetFunc = func(_ context.Context, id int64) (datapoints.Datapoint, error) {
		if id != 30411 {
			return datapoints.Datapoint{}, datapoints.ErrNotFound
		}
		return datapoints.Datapoint{Resolved: datapoints.Resolved{ID: id, SeriesKey: "yield.gsec10y", ObsDate: d, Value: num(6.7), Source: "override"}}, nil
	}
	retracted := false
	h.writer.RetractFunc = func(context.Context, pgx.Tx, datapoints.WriteMeta, []int64, string) error {
		if retracted {
			return datapoints.ErrAlreadyRetracted
		}
		retracted = true
		return nil
	}
	wantStatus(t, h.as(analystToken, req{method: "POST", path: "/v1/datapoints/30411/retract", op: "/v1/datapoints/{id}/retract", body: `{"reason":"Entered against wrong month"}`}), 200)
	wantCode(t, h.as(analystToken, req{method: "POST", path: "/v1/datapoints/30411/retract", op: "/v1/datapoints/{id}/retract", body: `{"reason":"again"}`}), 409, "conflict")
	wantCode(t, h.as(analystToken, req{method: "POST", path: "/v1/datapoints/999/retract", op: "/v1/datapoints/{id}/retract", body: `{"reason":"x"}`}), 404, "not_found")
	wantCode(t, h.as(analystToken, req{method: "POST", path: "/v1/datapoints/30411/retract", op: "/v1/datapoints/{id}/retract", body: `{"reason":"  "}`}), 400, "validation_failed")
}

func TestSnapshot(t *testing.T) {
	h := newHarness(t)
	h.cat.SeriesFunc = seriesLookup(testSeries())
	h.cat.ListSeriesFunc = func(_ context.Context, f catalog.SeriesFilter) (paging.Page[catalog.Series], error) {
		if len(f.Keys) != 1 || f.Keys[0] != "l1.*" {
			t.Fatalf("keys %v", f.Keys)
		}
		return paging.Page[catalog.Series]{Items: []catalog.Series{{Key: "l1.equity::valuation"}, {Key: "l1.metals::fundamentals"}}}, nil
	}
	h.reader.SnapshotFunc = func(_ context.Context, keys []string, _ civil.Date, o datapoints.ReadOpts) (map[string]datapoints.Resolved, error) {
		if !o.Cutoff.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("cutoff %v", o.Cutoff)
		}
		return map[string]datapoints.Resolved{"l1.equity::valuation": {ID: 41877, Value: num(45), Source: "computed"}}, nil
	}
	rec := h.as(viewerToken, req{method: "POST", path: "/v1/snapshot", op: "/v1/snapshot",
		body: `{"as_of":"2026-10-31","cutoff":"2026-11-01T00:00:00Z","series":["l1.*","pe.nifty100","nope.x"]}`})
	wantStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"missing":["l1.metals::fundamentals","nope.x","pe.nifty100"]`) {
		t.Fatal(rec.Body.String())
	}
}
