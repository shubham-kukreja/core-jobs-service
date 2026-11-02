package e2e

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"cloud.google.com/go/civil"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/app"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
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

// TestG1WriteToJobTree is gate G1 at the service level: a write lands rows
// and one outbox event, the dispatcher enqueues the subscriber's job, the job
// runs, and it is visible through jobs.Runs.
func TestG1WriteToJobTree(t *testing.T) {
	db := testdb.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	mustExec(t, db, `INSERT INTO catalog.sources(key,kind,name) VALUES ('analyst','analyst','Analyst')`)
	mustExec(t, db, `INSERT INTO catalog.series(key,name,layer,value_type,frequency,validation)
VALUES ('pe.nifty100','Nifty 100 P/E','raw','num','monthly','{"min":0,"max":200}')`)
	var userID int64
	if err := db.Owner.QueryRow(ctx,
		`INSERT INTO auth.users(email,name,role) VALUES ('analyst@example.com','Analyst','analyst') RETURNING id`).Scan(&userID); err != nil {
		t.Fatal(err)
	}

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
	defer func() { _ = w.Stop(context.Background()) }()

	var res datapoints.WriteResult
	err = database.InTx(ctx, db.Runtime, func(tx pgx.Tx) error {
		var werr error
		res, werr = core.Datapoints.Write(ctx, tx, datapoints.WriteMeta{Source: "analyst", ActorID: &userID},
			[]datapoints.Point{{SeriesKey: "pe.nifty100", ObsDate: civil.Date{Year: 2026, Month: 10, Day: 31}, Value: datapoints.Num(22.5)}})
		return werr
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Written != 1 || len(res.Rejected) != 0 {
		t.Fatalf("write: %+v", res)
	}

	select {
	case got := <-ran:
		if got != res.BatchID.String() {
			t.Fatalf("stub ran for batch %s, want %s", got, res.BatchID)
		}
	case <-ctx.Done():
		t.Fatal("subscriber job never ran")
	}

	// The run is visible, succeeded, and was triggered by the event.
	deadline := time.Now().Add(15 * time.Second)
	for {
		page, err := core.Runs.List(ctx, jobs.RunFilter{Kind: stubArgs{}.Kind()})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) == 1 && page.Items[0].Status == jobs.StatusSucceeded {
			run := page.Items[0]
			if run.Trigger != jobs.TriggerEvent {
				t.Fatalf("trigger = %s, want event", run.Trigger)
			}
			tree, err := core.Runs.Tree(ctx, run.UUID, 5)
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

func mustExec(t *testing.T, db *testdb.DB, sql string) {
	t.Helper()
	if _, err := db.Owner.Exec(context.Background(), sql); err != nil {
		t.Fatal(err)
	}
}
