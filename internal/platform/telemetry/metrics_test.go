package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
)

func TestJobAndHTTPMetrics(t *testing.T) {
	p := NewPrometheus()
	p.JobStarted("model.run", jobs.TriggerSchedule)
	p.JobStarted("model.run", jobs.TriggerUser)
	p.JobFinished("model.run", jobs.TriggerSchedule, jobs.StatusSucceeded, 2*time.Second)
	p.ObserveHTTP("GET", "/v1/runs/{id}", 200, 30*time.Millisecond)

	if got := testutil.ToFloat64(p.jobsRunning.WithLabelValues("model.run")); got != 1 {
		t.Errorf("running = %v", got)
	}
	if got := testutil.ToFloat64(p.jobsFinished.WithLabelValues("model.run", "schedule", "succeeded")); got != 1 {
		t.Errorf("finished = %v", got)
	}
	if got := testutil.ToFloat64(p.httpRequests.WithLabelValues("GET", "/v1/runs/{id}", "200")); got != 1 {
		t.Errorf("http = %v", got)
	}

	body := scrape(t, p)
	for _, want := range []string{
		`core_jobs_started_total{kind="model.run",trigger="schedule"} 1`,
		`core_jobs_duration_seconds_count{kind="model.run",status="succeeded"} 1`,
		`core_http_request_duration_seconds_count{method="GET",route="/v1/runs/{id}"} 1`,
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition lacks %q", want)
		}
	}
}

func TestBacklogCollector(t *testing.T) {
	p := NewPrometheus()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fail := false
	p.RegisterBacklog("jobs", "Open job runs.", []string{"kind", "status"}, func(context.Context) ([]Backlog, error) {
		if fail {
			return nil, errors.New("db down")
		}
		return []Backlog{{Labels: []string{"derive.cascade", "queued"}, Count: 3, OldestAge: 90 * time.Second}}, nil
	}, log)

	body := scrape(t, p)
	for _, want := range []string{
		`core_jobs_depth{kind="derive.cascade",status="queued"} 3`,
		`core_jobs_oldest_seconds{kind="derive.cascade",status="queued"} 90`,
		`core_jobs_up 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition lacks %q", want)
		}
	}

	// A failed read reports up=0 and still serves the rest of the scrape.
	fail = true
	body = scrape(t, p)
	if !strings.Contains(body, "core_jobs_up 0") || strings.Contains(body, "core_jobs_depth{") || !strings.Contains(body, "go_goroutines") {
		t.Fatalf("failed backlog scrape:\n%s", body)
	}
}

func scrape(t *testing.T, p *Prometheus) string {
	t.Helper()
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status %d", rec.Code)
	}
	return rec.Body.String()
}

func TestInitTracingDisabledWithoutEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	stop, err := InitTracing(t.Context(), "core-test", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := stop(t.Context()); err != nil {
		t.Fatal(err)
	}
}
