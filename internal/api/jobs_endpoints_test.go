package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/ingest"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
)

type qcArgs struct {
	AsOf   string   `json:"as_of,omitempty"`
	Checks []string `json:"checks,omitempty"`
}

func (qcArgs) Kind() string { return "quality.check" }

type sysArgs struct{}

func (sysArgs) Kind() string { return "outbox.dispatch" }

func registerKinds(r *jobs.Registry) {
	noop := func(context.Context, *jobs.RunCtx, qcArgs) (jobs.Outcome, error) { return jobs.Outcome{}, nil }
	jobs.Register(r, jobs.Spec{Kind: "quality.check", Queue: jobs.QueueCompute, Triggers: []jobs.TriggerType{jobs.TriggerUser, jobs.TriggerSchedule},
		UserTriggerable: true, Roles: []string{"analyst", "admin"}, MaxAttempts: 1, Timeout: time.Minute, ArgsVersion: 1}, jobs.HandlerFunc[qcArgs](noop))
	jobs.Register(r, jobs.Spec{Kind: "outbox.dispatch", Queue: jobs.QueueSystem, Triggers: []jobs.TriggerType{jobs.TriggerSystem},
		MaxAttempts: 1, Timeout: time.Minute, ArgsVersion: 1},
		jobs.HandlerFunc[sysArgs](func(context.Context, *jobs.RunCtx, sysArgs) (jobs.Outcome, error) { return jobs.Outcome{}, nil }))
}

var analystID = int64(2)

func sampleRun(id int64, owner *int64) jobs.JobRun {
	root := id
	return jobs.JobRun{ID: id, UUID: uuidOf(id), Kind: "quality.check", Args: json.RawMessage(`{"checks":["stale"]}`),
		Trigger: jobs.TriggerUser, UserID: owner, RootID: &root, Queue: jobs.QueueCompute, Status: jobs.StatusRunning,
		Attempt: 1, Progress: 40, Message: "checking", QueuedAt: testNow}
}

func TestJobsEndpoints(t *testing.T) {
	h := newHarness(t)
	registerKinds(h.kinds)

	wantStatus(t, h.do(req{method: "GET", path: "/v1/job-kinds", op: "/v1/job-kinds", token: viewerToken}), 200)

	var enqueued jobs.Args
	h.enq.EnqueueTxFunc = func(_ context.Context, _ pgx.Tx, a jobs.Args, _ ...jobs.Opt) (jobs.RunRef, error) {
		enqueued = a
		return jobs.RunRef{ID: 9, UUID: uuidOf(9), Status: jobs.StatusQueued}, nil
	}
	rec := h.as(analystToken, req{method: "POST", path: "/v1/jobs", op: "/v1/jobs", body: `{"kind":"quality.check","args":{"as_of":"2026-11-01","checks":["stale"]}}`})
	wantStatus(t, rec, 202)
	if a, ok := enqueued.(qcArgs); !ok || a.AsOf != "2026-11-01" {
		t.Fatalf("enqueued %#v", enqueued)
	}
	wantCode(t, h.as(viewerToken, req{method: "POST", path: "/v1/jobs", op: "/v1/jobs", body: `{"kind":"quality.check","args":{}}`}), 403, "forbidden")
	wantCode(t, h.as(adminToken, req{method: "POST", path: "/v1/jobs", op: "/v1/jobs", body: `{"kind":"outbox.dispatch","args":{}}`}), 400, "not_user_triggerable")
	wantCode(t, h.as(adminToken, req{method: "POST", path: "/v1/jobs", op: "/v1/jobs", body: `{"kind":"quality.check","args":{"bogus":1}}`}), 400, "validation_failed")
	wantCode(t, h.as(adminToken, req{method: "POST", path: "/v1/jobs", op: "/v1/jobs", body: `{"kind":"nope.kind","args":{}}`}), 400, "validation_failed")

	h.runs.GetFunc = func(_ context.Context, id uuid.UUID) (jobs.JobRun, error) {
		if id == uuidOf(5) {
			return sampleRun(5, &analystID), nil
		}
		return jobs.JobRun{}, jobs.ErrNotFound
	}
	h.runs.ListFunc = func(_ context.Context, f jobs.RunFilter) (paging.Page[jobs.JobRun], error) {
		return paging.Page[jobs.JobRun]{Items: []jobs.JobRun{sampleRun(5, &analystID)}}, nil
	}
	h.runs.TreeFunc = func(_ context.Context, id uuid.UUID, depth int) (jobs.RunTree, error) {
		child := jobs.RunTree{JobRun: jobs.JobRun{UUID: uuidOf(6), Kind: "derive.cascade", Trigger: jobs.TriggerEvent, Status: jobs.StatusSucceeded, Result: json.RawMessage(`{"series":3}`)}}
		return jobs.RunTree{JobRun: sampleRun(5, nil), Children: []jobs.RunTree{child}}, nil
	}
	wantStatus(t, h.do(req{method: "GET", path: "/v1/jobs?kind=quality.check&status=running", op: "/v1/jobs", token: viewerToken}), 200)
	wantCode(t, h.do(req{method: "GET", path: "/v1/jobs?status=weird", op: "/v1/jobs", token: viewerToken}), 400, "validation_failed")
	rec = h.do(req{method: "GET", path: "/v1/jobs/" + uuidOf(5).String(), op: "/v1/jobs/{id}", token: viewerToken})
	wantStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"user":"`+analystUser.Email+`"`) {
		t.Fatalf("owner email missing: %s", rec.Body)
	}
	wantCode(t, h.do(req{method: "GET", path: "/v1/jobs/" + uuidOf(7).String(), op: "/v1/jobs/{id}", token: viewerToken}), 404, "not_found")
	wantStatus(t, h.do(req{method: "GET", path: "/v1/jobs/" + uuidOf(5).String() + "/tree?depth=3", op: "/v1/jobs/{id}/tree", token: viewerToken}), 200)

	h.runs.RequestCancelFunc = func(context.Context, pgx.Tx, uuid.UUID) error { return nil }
	wantCode(t, h.as(viewerToken, req{method: "POST", path: "/v1/jobs/" + uuidOf(5).String() + "/cancel", op: "/v1/jobs/{id}/cancel"}), 403, "forbidden")
	wantStatus(t, h.as(analystToken, req{method: "POST", path: "/v1/jobs/" + uuidOf(5).String() + "/cancel", op: "/v1/jobs/{id}/cancel"}), 202)

	h.runs.RetryFunc = func(context.Context, pgx.Tx, uuid.UUID, int64) (jobs.RunRef, error) {
		return jobs.RunRef{ID: 10, UUID: uuidOf(10), Status: jobs.StatusQueued}, nil
	}
	wantCode(t, h.as(analystToken, req{method: "POST", path: "/v1/jobs/" + uuidOf(5).String() + "/retry", op: "/v1/jobs/{id}/retry"}), 403, "forbidden")
	wantStatus(t, h.as(adminToken, req{method: "POST", path: "/v1/jobs/" + uuidOf(5).String() + "/retry", op: "/v1/jobs/{id}/retry"}), 202)

	if n := len(h.audits); n != 3 {
		t.Fatalf("audits: %+v", h.audits)
	}
}

func TestJobEventsStream(t *testing.T) {
	h := newHarness(t)
	h.runs.WatchFunc = func(context.Context, uuid.UUID) (<-chan jobs.RunUpdate, error) {
		ch := make(chan jobs.RunUpdate, 2)
		ch <- jobs.RunUpdate{Kind: "progress", Progress: 40, Message: "tilt"}
		ch <- jobs.RunUpdate{Kind: "status", Status: jobs.StatusSucceeded, Progress: 100, Result: json.RawMessage(`{"ok":true}`)}
		close(ch)
		return ch, nil
	}
	srv := httptest.NewServer(h.h)
	defer srv.Close()
	r, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/jobs/"+uuidOf(5).String()+"/events", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: viewerToken})
	resp, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	want := "event: progress\ndata: {\"message\":\"tilt\",\"progress\":40}\n\nevent: status\ndata: {\"progress\":100,\"result\":{\"ok\":true},\"status\":\"succeeded\"}\n\n"
	if string(b) != want {
		t.Fatalf("stream:\n%q\nwant\n%q", b, want)
	}
}

func TestSchedulesEndpoints(t *testing.T) {
	h := newHarness(t)
	sc := jobs.Schedule{ID: 1, UUID: uuidOf(1), Name: "data-due-monthly", Kind: "quality.check", Args: json.RawMessage(`{}`),
		ScheduleType: "calendar", Expr: "business_day:12", Timezone: "Asia/Kolkata", OverlapPolicy: "skip", CatchupPolicy: "latest",
		Enabled: true, NextRunAt: testNow, LastRunID: ptr(int64(5))}
	h.scheds.ListFunc = func(context.Context) ([]jobs.Schedule, error) { return []jobs.Schedule{sc}, nil }
	h.runs.GetFunc = func(context.Context, uuid.UUID) (jobs.JobRun, error) { return sampleRun(5, nil), nil }
	h.scheds.GetFunc = func(_ context.Context, id uuid.UUID) (jobs.Schedule, error) {
		if id == sc.UUID {
			return sc, nil
		}
		return jobs.Schedule{}, jobs.ErrNotFound
	}
	h.scheds.SaveFunc = func(_ context.Context, _ pgx.Tx, in jobs.Schedule, _ int64) (jobs.Schedule, error) {
		in.UUID, in.NextRunAt = uuidOf(2), testNow
		return in, nil
	}
	h.scheds.SetEnabledFunc = func(context.Context, pgx.Tx, uuid.UUID, bool) error { return nil }
	h.scheds.PreviewFunc = func(_ context.Context, id uuid.UUID, _ int) ([]time.Time, error) {
		if id != sc.UUID {
			return nil, jobs.ErrNotFound
		}
		return []time.Time{testNow}, nil
	}
	h.scheds.RunNowFunc = func(context.Context, pgx.Tx, uuid.UUID, int64) (jobs.RunRef, error) {
		return jobs.RunRef{ID: 11, UUID: uuidOf(11), Status: jobs.StatusQueued}, nil
	}
	registerKinds(h.kinds)
	id := sc.UUID.String()
	wantStatus(t, h.do(req{method: "GET", path: "/v1/schedules", op: "/v1/schedules", token: viewerToken}), 200)
	body := `{"name":"data-due-monthly","kind":"quality.check","args":{"as_of":"$prev_month_end"},"schedule_type":"calendar","expr":"business_day:12"}`
	wantStatus(t, h.as(adminToken, req{method: "POST", path: "/v1/schedules", op: "/v1/schedules", body: body}), 201)
	wantCode(t, h.as(analystToken, req{method: "POST", path: "/v1/schedules", op: "/v1/schedules", body: body}), 403, "forbidden")
	wantCode(t, h.as(adminToken, req{method: "POST", path: "/v1/schedules", op: "/v1/schedules", body: `{"name":"Bad Name"}`}), 400, "validation_failed")
	wantStatus(t, h.as(adminToken, req{method: "PATCH", path: "/v1/schedules/" + id, op: "/v1/schedules/{id}", body: `{"jitter_secs":60}`}), 200)
	wantCode(t, h.as(adminToken, req{method: "PATCH", path: "/v1/schedules/" + id, op: "/v1/schedules/{id}", body: `{}`}), 400, "validation_failed")
	wantStatus(t, h.as(adminToken, req{method: "POST", path: "/v1/schedules/" + id + "/pause", op: "/v1/schedules/{id}/pause"}), 200)
	wantStatus(t, h.as(adminToken, req{method: "POST", path: "/v1/schedules/" + id + "/resume", op: "/v1/schedules/{id}/resume"}), 200)
	wantStatus(t, h.as(adminToken, req{method: "POST", path: "/v1/schedules/" + id + "/run-now", op: "/v1/schedules/{id}/run-now"}), 202)
	wantStatus(t, h.do(req{method: "GET", path: "/v1/schedules/" + id + "/preview?n=3", op: "/v1/schedules/{id}/preview", token: viewerToken}), 200)
	wantCode(t, h.do(req{method: "GET", path: "/v1/schedules/" + uuidOf(9).String() + "/preview", op: "/v1/schedules/{id}/preview", token: viewerToken}), 404, "not_found")
}

func ptr[T any](v T) *T { return &v }

func TestUploadsAndMappings(t *testing.T) {
	h := newHarness(t, func(c *Config, _ *Deps) { c.MaxUploadBytes = 1 << 20 })
	h.uploads.StoreFunc = func(_ context.Context, in ingest.UploadInput) (ingest.Upload, error) {
		b, _ := io.ReadAll(in.Body)
		return ingest.Upload{UUID: uuidOf(3), Filename: in.Filename, ContentType: ingest.MediaCSV, Bytes: int64(len(b)),
			SHA256: bytes.Repeat([]byte{0xab}, 32), CreatedAt: testNow}, nil
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "rates.csv")
	_, _ = fw.Write([]byte("Month,10Y\nJan-2026,6.7\n"))
	_ = mw.Close()
	rec := h.as(analystToken, req{method: "POST", path: "/v1/uploads", op: "/v1/uploads", body: buf.String(),
		headers: map[string]string{"Content-Type": mw.FormDataContentType()}})
	wantStatus(t, rec, 201)
	wantCode(t, h.as(analystToken, req{method: "POST", path: "/v1/uploads", op: "/v1/uploads", body: `{}`}), 415, "unsupported_media_type")

	m := ingest.ImportMapping{Key: "data_trackers_rates", Name: "Data Trackers – rates", Version: 1, CreatedAt: testNow, UpdatedAt: testNow,
		Mapping: ingest.Mapping{HeaderRowMatch: "Month", DateColumn: ingest.DateColumn{Name: "Month", Format: "Jan-2006"},
			Columns: []ingest.ColumnMap{{Name: "10Y", Series: "yield.gsec10y"}}, KnownAt: "now"}}
	h.maps.SaveFunc = func(_ context.Context, key, name string, _ ingest.Mapping, _ int64) (ingest.ImportMapping, error) {
		if key != m.Key {
			t.Errorf("derived key %q", key)
		}
		return m, nil
	}
	h.maps.ListFunc = func(context.Context) ([]ingest.ImportMapping, error) { return []ingest.ImportMapping{m}, nil }
	wantStatus(t, h.as(analystToken, req{method: "POST", path: "/v1/import-mappings", op: "/v1/import-mappings",
		body: `{"name":"Data Trackers – rates","header_row_match":"Month","date_column":{"name":"Month","format":"Jan-2006"},"columns":[{"name":"10Y","series":"yield.gsec10y"}],"known_at":"now"}`}), 201)
	wantStatus(t, h.do(req{method: "GET", path: "/v1/import-mappings", op: "/v1/import-mappings", token: viewerToken}), 200)

	off := newHarness(t, func(_ *Config, d *Deps) { d.Jobs.Uploads, d.Jobs.Mappings = nil, nil })
	wantCode(t, off.do(req{method: "GET", path: "/v1/import-mappings", token: viewerToken}), 409, "not_available")
}

func TestQualityReport(t *testing.T) {
	h := newHarness(t)
	h.runs.ListFunc = func(_ context.Context, f jobs.RunFilter) (paging.Page[jobs.JobRun], error) {
		if f.Kind != "quality.check" || f.Status != jobs.StatusSucceeded {
			t.Errorf("filter %+v", f)
		}
		return paging.Page[jobs.JobRun]{Items: []jobs.JobRun{{Result: json.RawMessage(
			`{"as_of":"2026-11-01","stale":[{"series":"flows.small","last_obs":"2026-09-30","expected_by":"2026-10-31"}],"outliers":[],"insufficient_history":[],"missing_scores":[],"bad_categories":[]}`)}}}, nil
	}
	wantStatus(t, h.do(req{method: "GET", path: "/v1/quality/report", op: "/v1/quality/report", token: viewerToken}), 200)
	wantStatus(t, h.do(req{method: "GET", path: "/v1/quality/report?as_of=2026-11-01", op: "/v1/quality/report", token: viewerToken}), 200)
	wantCode(t, h.do(req{method: "GET", path: "/v1/quality/report?as_of=2026-10-01", op: "/v1/quality/report", token: viewerToken}), 404, "not_found")
	wantCode(t, h.do(req{method: "GET", path: "/v1/quality/report?as_of=nope", op: "/v1/quality/report", token: viewerToken}), 400, "invalid_params")
}
