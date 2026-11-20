package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/shubham-kukreja/core-jobs-service/internal/app"
	"github.com/shubham-kukreja/core-jobs-service/internal/auth"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/seed"
)

// TestPipelineFakeClock is P4-T7-05: the monthly cycle end to end, with the
// seeded schedules driven by a fake clock. The clock is shifted, not frozen:
// it runs from April 2026 and jumps between calendar dates, so writes get
// distinct known_at values as they would in production.
//
//	Apr 6   analyst uploads the trackers and extras over HTTP → import.file
//	        → derive.cascade → veto.evaluate (event-triggered)
//	Apr 16  business day 12: data-due quality.check and veto-monthly fire
//	Apr 20  quarterly review: model.run fires and stores a draft run
//	        analyst approves, admin publishes, /allocations/current serves it
//
// The shipped review calendar starts in December 2026, after the workbook's
// data ends, so the test moves the first quarterly review to 2026-04-20.
func TestPipelineFakeClock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewShifted(time.Date(2026, 4, 6, 10, 0, 0, 0, ist))
	g := newG2(ctx, t, clk, func(docs seed.Documents) {
		cfg := docs["model_maaf_v1.yaml"].(map[string]any)["config"].(map[string]any)
		cfg["review_calendar"] = map[string]any{
			"quarterly":  []any{"2026-04-20", "2026-06-30"},
			"semiannual": []any{"2026-06-30", "2026-12-31"},
		}
	})
	series, err := seed.SeriesInputs(g.docs)
	if err != nil {
		t.Fatal(err)
	}
	api := newAPIClient(ctx, t, g, clk)

	// Apr 6: uploads over HTTP, then the analyst's entries.
	trackers := xlsx(t, "Data Trackers", dataTrackersGrid())
	api.importFile(t, "MultiAsset_Allocation_Framework_v2.xlsx", trackers,
		"data_trackers.valuation", "data_trackers.rates", "data_trackers.metals", "data_trackers.flows")
	api.importFile(t, "extras.xlsx", xlsx(t, "Extras", extrasGrid(g.mapping(t, "extras_log.yaml"))), "upload.extras")
	g.writeAnalyst(ctx, t, series)
	if n := g.count(ctx, t, `SELECT count(*) FROM jobs.job_runs WHERE kind = 'veto.evaluate' AND trigger = 'event' AND status = 'succeeded'`); n == 0 {
		t.Fatal("trigger writes did not run veto.evaluate")
	}
	if n := g.count(ctx, t, `SELECT count(*) FROM jobs.job_runs WHERE trigger = 'schedule'`); n != 0 {
		t.Fatalf("%d scheduled runs before any slot was due", n)
	}

	// Apr 16 (business day 12): data-due check and veto-monthly.
	g.tick(ctx, t, clk, time.Date(2026, 4, 16, 10, 0, 0, 0, ist))
	for _, kind := range []string{"quality.check", "veto.evaluate"} {
		if n := g.count(ctx, t, `SELECT count(*) FROM jobs.job_runs WHERE trigger = 'schedule' AND status = 'succeeded' AND kind = '`+kind+`'`); n == 0 {
			t.Errorf("no scheduled %s ran by Apr 16", kind)
		}
	}
	if n := g.count(ctx, t, `SELECT count(*) FROM jobs.job_runs WHERE kind = 'model.run'`); n != 0 {
		t.Fatalf("model.run fired before the review date")
	}
	var quality struct {
		AsOf string `json:"as_of"`
	}
	api.getJSON(t, "/v1/quality/report?as_of=2026-03-31", &quality)

	// Apr 20: the quarterly review stores a draft run.
	g.tick(ctx, t, clk, time.Date(2026, 4, 20, 9, 0, 0, 0, ist))
	var runs struct {
		Data []struct {
			ID      string `json:"id"`
			Status  string `json:"status"`
			AsOf    string `json:"as_of"`
			Trigger string `json:"trigger"`
		} `json:"data"`
	}
	api.getJSON(t, "/v1/runs?model=maaf&status=draft", &runs)
	if len(runs.Data) != 1 || runs.Data[0].AsOf != "2026-04-20" {
		t.Fatalf("draft runs = %+v", runs.Data)
	}
	runID := runs.Data[0].ID
	if n := g.count(ctx, t, `SELECT count(*) FROM jobs.job_runs WHERE kind = 'model.run' AND trigger = 'schedule' AND status = 'succeeded'`); n != 1 {
		t.Fatalf("scheduled model.run count = %d", n)
	}

	// Approve (analyst) and publish (admin); an analyst cannot publish.
	api.do(t, api.analyst, http.MethodPost, "/v1/runs/"+runID+"/approve", `{"note":"Q1 review"}`, http.StatusOK)
	api.do(t, api.analyst, http.MethodPost, "/v1/runs/"+runID+"/publish", "", http.StatusForbidden)
	api.do(t, api.admin, http.MethodPost, "/v1/runs/"+runID+"/publish", "", http.StatusOK)

	var cur struct {
		RunID string `json:"run_id"`
		AsOf  string `json:"as_of"`
		Total string `json:"total"`
		Lines []struct {
			LineItem        string `json:"line_item"`
			PortfolioWeight string `json:"portfolio_weight"`
		} `json:"lines"`
	}
	api.getJSON(t, "/v1/allocations/current?model=maaf&profile=balanced", &cur)
	sum := decimal.Zero
	for _, l := range cur.Lines {
		sum = sum.Add(decimal.RequireFromString(l.PortfolioWeight))
	}
	if cur.RunID != runID || len(cur.Lines) == 0 || !sum.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("current allocation = %+v (sum %s)", cur, sum)
	}

	// Every job in the cycle succeeded and is visible per kind in metrics.
	if n := g.count(ctx, t, `SELECT count(*) FROM jobs.job_runs WHERE status NOT IN ('succeeded', 'skipped')`); n != 0 {
		t.Errorf("%d runs did not succeed", n)
	}
	metrics := scrapeMetrics(t, g.core)
	for _, want := range []string{
		`core_jobs_finished_total{kind="import.file",status="succeeded",trigger="user"}`,
		`core_jobs_finished_total{kind="derive.cascade",status="succeeded",trigger="event"}`,
		`core_jobs_finished_total{kind="veto.evaluate",status="succeeded",trigger="schedule"}`,
		`core_jobs_finished_total{kind="quality.check",status="succeeded",trigger="schedule"}`,
		`core_jobs_finished_total{kind="model.run",status="succeeded",trigger="schedule"} 1`,
		`core_http_requests_total{code="200",method="POST",route="/v1/runs/{id}/publish"} 1`,
		`core_outbox_depth 0`,
		`core_jobs_up 1`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}

// tick moves the fake clock, fires due schedules as the worker's periodic
// tick would, and waits for everything to finish.
func (g *g2) tick(ctx context.Context, t *testing.T, clk *clock.Shifted, to time.Time) {
	t.Helper()
	clk.Set(to)
	sched := jobs.NewScheduler(g.core.Pool, g.core.ScheduleTypes, g.core.Jobs, clk, g.core.Log)
	if _, err := sched.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	g.drain(ctx, t)
}

func scrapeMetrics(t *testing.T, core *app.Core) string {
	t.Helper()
	rec := httptest.NewRecorder()
	core.Metrics.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// apiClient calls the real /v1 handler with API keys.
type apiClient struct {
	ctx            context.Context
	g              *g2
	srv            *httptest.Server
	admin, analyst string
}

func newAPIClient(ctx context.Context, t *testing.T, g *g2, clk clock.Clock) *apiClient {
	t.Helper()
	authSvc, _, err := app.NewAuth(g.db.Runtime, testConfig(), clk)
	if err != nil {
		t.Fatal(err)
	}
	adminKey, _, err := authSvc.CreateAPIKey(ctx, g.uid, "pipeline admin", 0)
	if err != nil {
		t.Fatal(err)
	}
	an, err := authSvc.Create(ctx, "analyst@example.com", "Analyst", auth.RoleAnalyst)
	if err != nil {
		t.Fatal(err)
	}
	analystKey, _, err := authSvc.CreateAPIKey(ctx, an.ID, "pipeline analyst", 0)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := g.core.NewAPI(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(v1)
	t.Cleanup(srv.Close)
	return &apiClient{ctx: ctx, g: g, srv: srv, admin: adminKey, analyst: analystKey}
}

func (c *apiClient) send(t *testing.T, key, method, path, contentType string, body io.Reader, want int) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(c.ctx, method, c.srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s: %d, want %d: %s", method, path, resp.StatusCode, want, out)
	}
	return out
}

func (c *apiClient) do(t *testing.T, key, method, path, body string, want int) []byte {
	t.Helper()
	var r io.Reader
	ct := ""
	if body != "" {
		r, ct = strings.NewReader(body), "application/json"
	}
	return c.send(t, key, method, path, ct, r, want)
}

func (c *apiClient) getJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := json.Unmarshal(c.do(t, c.analyst, http.MethodGet, path, "", http.StatusOK), v); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
}

// importFile uploads data as the analyst and submits import.file for each
// mapping through POST /v1/jobs, waiting for each to finish cleanly.
func (c *apiClient) importFile(t *testing.T, name string, data []byte, mappings ...string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	var up struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(c.send(t, c.analyst, http.MethodPost, "/v1/uploads", mw.FormDataContentType(), &buf, http.StatusCreated), &up); err != nil {
		t.Fatal(err)
	}
	for _, m := range mappings {
		body := fmt.Sprintf(`{"kind":"import.file","args":{"upload_id":%q,"mapping_id":%q}}`, up.ID, m)
		var job struct {
			Run struct {
				ID string `json:"id"`
			} `json:"job_run"`
		}
		if err := json.Unmarshal(c.do(t, c.analyst, http.MethodPost, "/v1/jobs", body, http.StatusAccepted), &job); err != nil {
			t.Fatal(err)
		}
		c.g.drain(c.ctx, t)
		var run struct {
			Status string `json:"status"`
			Result struct {
				RowsWritten int   `json:"rows_written"`
				Rejected    []any `json:"rejected"`
			} `json:"result"`
		}
		c.getJSON(t, "/v1/jobs/"+job.Run.ID, &run)
		if run.Status != "succeeded" || run.Result.RowsWritten == 0 || len(run.Result.Rejected) > 0 {
			t.Fatalf("import %s: %+v", m, run)
		}
	}
}
