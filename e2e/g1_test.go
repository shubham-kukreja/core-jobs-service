package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/civil"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/app"
	"github.com/shubham-kukreja/core-jobs-service/internal/auth"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/config"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/testdb"
)

// stubArgs and stubSubscriber stand in for derive.cascade until Phase 2:
// gate G1 only proves the event path, not the computation.
type stubArgs struct {
	BatchID string `json:"batch_id"`
}

func (stubArgs) Kind() string { return "test.g1_stub" }

type stubSubscriber struct{}

func (stubSubscriber) Topic() string { return datapoints.TopicWritten }

func (stubSubscriber) Handle(_ context.Context, ev jobs.Event) ([]jobs.Args, error) {
	var w datapoints.WrittenEvent
	if err := json.Unmarshal(ev.Payload, &w); err != nil {
		return nil, err
	}
	return []jobs.Args{stubArgs{BatchID: w.BatchID.String()}}, nil
}

type g1 struct {
	db   *testdb.DB
	core *app.Core
	ran  chan string
}

// setupG1 seeds one source and series, builds the core with the stub kind
// registered, and starts a worker.
func setupG1(ctx context.Context, t *testing.T) *g1 {
	t.Helper()
	db := testdb.New(t)
	mustExec(t, db, `INSERT INTO catalog.sources(key,kind,name) VALUES ('analyst','analyst','Analyst')`)
	mustExec(t, db, `INSERT INTO catalog.series(key,name,layer,value_type,frequency,validation)
VALUES ('pe.nifty100','Nifty 100 P/E','raw','num','monthly','{"min":0,"max":200}')`)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	core, err := app.NewCore(db.Runtime, log, clock.System{})
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan string, 1)
	jobs.Register(core.Registry, jobs.Spec{
		Kind: stubArgs{}.Kind(), Queue: jobs.QueueCompute, Triggers: []jobs.TriggerType{jobs.TriggerEvent},
		MaxAttempts: 1, Timeout: 10 * time.Second, ArgsVersion: 1,
	}, jobs.HandlerFunc[stubArgs](func(_ context.Context, _ *jobs.RunCtx, a stubArgs) (jobs.Outcome, error) {
		ran <- a.BatchID
		return jobs.Outcome{Summary: map[string]any{"batch_id": a.BatchID}}, nil
	}))
	w, err := core.NewWorker(stubSubscriber{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Stop(context.Background()) })
	return &g1{db: db, core: core, ran: ran}
}

// awaitRun waits for the stub job for batchID to run and checks it is
// visible as a succeeded, event-triggered run with a tree.
func (g *g1) awaitRun(ctx context.Context, t *testing.T, batchID string) {
	t.Helper()
	select {
	case got := <-g.ran:
		if got != batchID {
			t.Fatalf("stub ran for batch %s, want %s", got, batchID)
		}
	case <-ctx.Done():
		t.Fatal("subscriber job never ran")
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		page, err := g.core.Runs.List(ctx, jobs.RunFilter{Kind: stubArgs{}.Kind()})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) == 1 && page.Items[0].Status == jobs.StatusSucceeded {
			run := page.Items[0]
			if run.Trigger != jobs.TriggerEvent {
				t.Fatalf("trigger = %s, want event", run.Trigger)
			}
			tree, err := g.core.Runs.Tree(ctx, run.UUID, 5)
			if err != nil || tree.UUID != run.UUID {
				t.Fatalf("tree: %+v %v", tree, err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run not succeeded: %+v", page.Items)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestG1WriteToJobTree is gate G1 at the service level.
func TestG1WriteToJobTree(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	g := setupG1(ctx, t)

	var userID int64
	if err := g.db.Owner.QueryRow(ctx,
		`INSERT INTO auth.users(email,name,role) VALUES ('analyst@example.com','Analyst','analyst') RETURNING id`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	var res datapoints.WriteResult
	err := database.InTx(ctx, g.db.Runtime, func(tx pgx.Tx) error {
		var werr error
		res, werr = g.core.Datapoints.Write(ctx, tx, datapoints.WriteMeta{Source: "analyst", ActorID: &userID},
			[]datapoints.Point{{SeriesKey: "pe.nifty100", ObsDate: civil.Date{Year: 2026, Month: 10, Day: 31}, Value: datapoints.Num(22.5)}})
		return werr
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Written != 1 || len(res.Rejected) != 0 {
		t.Fatalf("write: %+v", res)
	}
	g.awaitRun(ctx, t, res.BatchID.String())
}

// TestG1OverHTTP is gate G1 as specified: log in, POST /v1/datapoints with
// the session cookie and CSRF token, and the event-triggered job runs.
func TestG1OverHTTP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	g := setupG1(ctx, t)

	authSvc, _, err := app.NewAuth(g.db.Runtime, testConfig(), clock.System{})
	if err != nil {
		t.Fatal(err)
	}
	const password = "correct horse battery staple 42"
	u, err := authSvc.Create(ctx, "analyst@example.com", "Analyst", auth.RoleAnalyst)
	if err != nil {
		t.Fatal(err)
	}
	if err := authSvc.SetPassword(ctx, u.ID, password); err != nil {
		t.Fatal(err)
	}
	v1, err := g.core.NewAPI(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(v1)
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}

	login := post(ctx, t, client, srv.URL+"/v1/auth/login", "",
		`{"email":"analyst@example.com","password":"`+password+`"}`)
	defer func() { _ = login.Body.Close() }()
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login: %d %s", login.StatusCode, body(t, login))
	}
	var csrf string
	for _, c := range login.Cookies() {
		if c.Name == "core_csrf" {
			csrf = c.Value
		}
	}
	if csrf == "" {
		t.Fatal("login did not set core_csrf")
	}

	write := `{"source":"analyst","points":[{"series":"pe.nifty100","obs_date":"2026-10-31","value":22.5}]}`
	noCSRF := post(ctx, t, client, srv.URL+"/v1/datapoints", "", write)
	_ = noCSRF.Body.Close()
	if noCSRF.StatusCode != http.StatusForbidden {
		t.Fatalf("write without CSRF token: want 403, got %d", noCSRF.StatusCode)
	}
	r := post(ctx, t, client, srv.URL+"/v1/datapoints", csrf, write)
	defer func() { _ = r.Body.Close() }()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("write: %d %s", r.StatusCode, body(t, r))
	}
	var res struct {
		BatchID string `json:"batch_id"`
		Written int    `json:"written"`
	}
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil || res.Written != 1 {
		t.Fatalf("write result: %+v %v", res, err)
	}
	g.awaitRun(ctx, t, res.BatchID)
}

func testConfig() config.Config {
	return config.Config{
		Env:                config.EnvTest,
		MaxRequestBytes:    1 << 20,
		SessionIdleTTL:     30 * time.Minute,
		SessionAbsoluteTTL: 12 * time.Hour,
		SecretKey:          config.Secret(strings.Repeat("t", 32)),
	}
}

func post(ctx context.Context, t *testing.T, c *http.Client, url, csrf, payload string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBufferString(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func body(t *testing.T, r *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

func mustExec(t *testing.T, db *testdb.DB, sql string) {
	t.Helper()
	if _, err := db.Owner.Exec(context.Background(), sql); err != nil {
		t.Fatal(err)
	}
}
