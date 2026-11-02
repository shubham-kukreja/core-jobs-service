package river_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	jriver "github.com/shubham-kukreja/core-jobs-service/internal/jobs/river"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/testdb"
)

type echoArgs struct {
	N int `json:"n"`
}

func (echoArgs) Kind() string { return "test.echo" }

type sleepArgs struct{}

func (sleepArgs) Kind() string { return "test.sleep" }

type stack struct {
	pool   *pgxpool.Pool
	reg    *jobs.Registry
	svc    *jobs.Service
	runs   *jobs.RunStore
	worker *jriver.Worker
	userID int64
	echoes atomic.Int32
}

// newStack wires the jobs module against a fresh database with the runtime
// role's privileges, as cmd/app does.
func newStack(t *testing.T, outbox jobs.OutboxSource, subs ...jobs.Subscriber) *stack {
	t.Helper()
	tdb := testdb.New(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	var uid int64
	if err := tdb.Owner.QueryRow(ctx,
		`INSERT INTO auth.users(email,name,role) VALUES ('t@x.co','T','admin') RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}

	s := &stack{pool: tdb.Runtime, reg: jobs.NewRegistry(), userID: uid}
	jobs.Register(s.reg, jobs.Spec{
		Kind: "test.echo", Queue: jobs.QueueCompute,
		Triggers:        []jobs.TriggerType{jobs.TriggerUser, jobs.TriggerSchedule, jobs.TriggerEvent},
		UserTriggerable: true, Roles: []string{"analyst"}, MaxAttempts: 3, Timeout: time.Minute, ArgsVersion: 1,
		Unique: &jobs.UniqueOpts{ByArgs: []string{"n"}, Period: time.Hour}, MaxPending: 3,
	}, jobs.HandlerFunc[echoArgs](func(_ context.Context, _ *jobs.RunCtx, a echoArgs) (jobs.Outcome, error) {
		s.echoes.Add(1)
		out := jobs.Outcome{Summary: map[string]any{"n": a.N}}
		if a.N == 1 {
			out.Next = []jobs.Args{echoArgs{N: 100}}
		}
		return out, nil
	}))
	jobs.Register(s.reg, jobs.Spec{
		Kind: "test.sleep", Queue: jobs.QueueCompute, Triggers: []jobs.TriggerType{jobs.TriggerUser},
		UserTriggerable: true, Roles: []string{"analyst"}, MaxAttempts: 1, Timeout: time.Minute, ArgsVersion: 1,
	}, jobs.HandlerFunc[sleepArgs](func(ctx context.Context, _ *jobs.RunCtx, _ sleepArgs) (jobs.Outcome, error) {
		<-ctx.Done()
		return jobs.Outcome{}, ctx.Err()
	}))

	eng, err := jriver.NewEngine(s.pool, log)
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.System{}
	s.svc = jobs.NewService(s.pool, s.reg, eng, clk)
	s.runs = jobs.NewRunStore(s.pool, s.svc, clk)
	if outbox == nil {
		outbox = emptyOutbox{}
	}
	s.worker, err = jriver.NewWorker(s.pool, jriver.WorkerDeps{
		Registry:   s.reg,
		Executor:   jobs.NewExecutor(s.pool, s.reg, s.svc, jobs.ExecutorConfig{CancelPoll: 100 * time.Millisecond, Log: log}),
		Scheduler:  jobs.NewScheduler(s.pool, jobs.DefaultScheduleTypes(), s.svc, clk, log),
		Dispatcher: jobs.NewDispatcher(s.pool, outbox, jobs.NewSubscribers(subs...), s.svc, log),
		Log:        log,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *stack) start(t *testing.T) {
	t.Helper()
	if err := s.worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.worker.Stop(ctx)
	})
}

func (s *stack) waitStatus(t *testing.T, id uuid.UUID, want jobs.Status) jobs.JobRun {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		r, err := s.runs.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status == want {
			return r
		}
		time.Sleep(50 * time.Millisecond)
	}
	r, _ := s.runs.Get(context.Background(), id)
	t.Fatalf("run %s: status %s, want %s", id, r.Status, want)
	return r
}

func TestRunSucceedsAndChains(t *testing.T) {
	s := newStack(t, nil)
	s.start(t)
	ref, err := s.svc.Enqueue(context.Background(), echoArgs{N: 1}, jobs.WithTrigger(jobs.TriggerUser), jobs.WithUser(s.userID))
	if err != nil {
		t.Fatal(err)
	}
	r := s.waitStatus(t, ref.UUID, jobs.StatusSucceeded)
	if r.Progress != 100 || string(r.Result) != `{"n": 1}` && string(r.Result) != `{"n":1}` {
		t.Fatalf("run = %+v result=%s", r, r.Result)
	}
	tree, err := s.runs.Tree(context.Background(), ref.UUID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Children) != 1 || tree.Children[0].Trigger != jobs.TriggerChain {
		t.Fatalf("tree = %+v", tree)
	}
	s.waitStatus(t, tree.Children[0].UUID, jobs.StatusSucceeded)
}

func TestIdempotencyAndUniqueness(t *testing.T) {
	s := newStack(t, nil) // not started: runs stay queued
	ctx := context.Background()
	user := []jobs.Opt{jobs.WithTrigger(jobs.TriggerUser), jobs.WithUser(s.userID)}

	a, err := s.svc.Enqueue(ctx, echoArgs{N: 5}, append(user, jobs.WithIdempotencyKey("k1"))...)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.svc.Enqueue(ctx, echoArgs{N: 5}, append(user, jobs.WithIdempotencyKey("k1"))...)
	if err != nil || !b.Duplicate || b.ID != a.ID {
		t.Fatalf("idempotent replay: %+v %v", b, err)
	}
	if _, err := s.svc.Enqueue(ctx, echoArgs{N: 6}, append(user, jobs.WithIdempotencyKey("k1"))...); !apperr.Is(err, apperr.IdempotencyMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	// Same n without a key: deduplicated by UniqueOpts.ByArgs.
	c, err := s.svc.Enqueue(ctx, echoArgs{N: 5}, user...)
	if err != nil || !c.Duplicate || c.ID != a.ID {
		t.Fatalf("unique: %+v %v", c, err)
	}
}

func TestMaxPendingAndValidation(t *testing.T) {
	s := newStack(t, nil)
	ctx := context.Background()
	user := []jobs.Opt{jobs.WithTrigger(jobs.TriggerUser), jobs.WithUser(s.userID)}
	for n := range 3 {
		if _, err := s.svc.Enqueue(ctx, echoArgs{N: 10 + n}, user...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.svc.Enqueue(ctx, echoArgs{N: 99}, user...); !apperr.Is(err, apperr.QueueFull) {
		t.Fatalf("4th submit: %v", err)
	}
	bad := jobs.RawArgs{KindName: "test.echo", JSON: json.RawMessage(`{"n":1,"extra":true}`)}
	if _, err := s.svc.Enqueue(ctx, bad, user...); !apperr.Is(err, apperr.ValidationFailed) {
		t.Fatalf("unknown field: %v", err)
	}
}

func TestCancelQueuedAndRunning(t *testing.T) {
	s := newStack(t, nil)
	ctx := context.Background()
	user := []jobs.Opt{jobs.WithTrigger(jobs.TriggerUser), jobs.WithUser(s.userID)}

	queued, err := s.svc.Enqueue(ctx, echoArgs{N: 42}, user...)
	if err != nil {
		t.Fatal(err)
	}
	cancelRun(t, s, queued.UUID)
	s.start(t)
	s.waitStatus(t, queued.UUID, jobs.StatusCancelled)

	running, err := s.svc.Enqueue(ctx, sleepArgs{}, user...)
	if err != nil {
		t.Fatal(err)
	}
	s.waitStatus(t, running.UUID, jobs.StatusRunning)
	start := time.Now()
	cancelRun(t, s, running.UUID)
	s.waitStatus(t, running.UUID, jobs.StatusCancelled)
	if time.Since(start) > 3*time.Second {
		t.Fatalf("cancel took %s", time.Since(start))
	}
	if s.echoes.Load() != 0 {
		t.Fatal("cancelled-while-queued run must not execute")
	}
}

func cancelRun(t *testing.T, s *stack, id uuid.UUID) {
	t.Helper()
	if err := database.InTx(context.Background(), s.pool, func(tx pgx.Tx) error {
		return s.runs.RequestCancel(context.Background(), tx, id)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerFiresOncePerSlot(t *testing.T) {
	s := newStack(t, nil)
	ctx := context.Background()
	store := jobs.NewScheduleStore(s.pool, s.reg, jobs.DefaultScheduleTypes(), s.svc, clock.System{})
	var sch jobs.Schedule
	if err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		sch, err = store.Save(ctx, tx, jobs.Schedule{
			Name: "echo-every-minute", Kind: "test.echo", Args: json.RawMessage(`{"n":7}`),
			ScheduleType: "interval", Expr: "1m", Enabled: true,
		}, s.userID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Make it due now, then tick twice at the same instant.
	if _, err := s.pool.Exec(ctx, `UPDATE jobs.job_schedules SET next_run_at = now() - interval '1 second' WHERE id = $1`, sch.ID); err != nil {
		t.Skipf("runtime role cannot backdate schedules: %v", err)
	}
	sched := jobs.NewScheduler(s.pool, jobs.DefaultScheduleTypes(), s.svc, clock.System{}, slog.New(slog.DiscardHandler))
	first, err := sched.Tick(ctx)
	if err != nil || first != 1 {
		t.Fatalf("first tick: %d %v", first, err)
	}
	second, err := sched.Tick(ctx)
	if err != nil || second != 0 {
		t.Fatalf("second tick fired again: %d %v", second, err)
	}
}

// Outbox: each event fans out exactly once across repeated dispatches.
func TestDispatchExactlyOnce(t *testing.T) {
	src := &memOutbox{events: []jobs.Event{{ID: 1, Topic: "t"}, {ID: 2, Topic: "t"}}}
	s := newStack(t, src, topicSub{})
	d := jobs.NewDispatcher(s.pool, src, jobs.NewSubscribers(topicSub{}), s.svc, slog.New(slog.DiscardHandler))
	ctx := context.Background()
	n1, err := d.Dispatch(ctx)
	if err != nil || n1 != 2 {
		t.Fatalf("first: %d %v", n1, err)
	}
	n2, err := d.Dispatch(ctx)
	if err != nil || n2 != 0 {
		t.Fatalf("second: %d %v", n2, err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM jobs.job_runs WHERE trigger = 'event'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("event runs = %d %v", count, err)
	}
}

type topicSub struct{}

func (topicSub) Topic() string { return "t" }
func (topicSub) Handle(_ context.Context, ev jobs.Event) ([]jobs.Args, error) {
	return []jobs.Args{echoArgs{N: 1000 + int(ev.ID)}}, nil
}

// memOutbox stands in for datapoints.Outbox; dispatch state commits with the tx.
type memOutbox struct {
	events     []jobs.Event
	dispatched map[int64]bool
}

func (m *memOutbox) Claim(context.Context, pgx.Tx, int) ([]jobs.Event, error) {
	var out []jobs.Event
	for _, e := range m.events {
		if !m.dispatched[e.ID] {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *memOutbox) MarkDispatched(_ context.Context, _ pgx.Tx, ids []int64) error {
	if m.dispatched == nil {
		m.dispatched = map[int64]bool{}
	}
	for _, id := range ids {
		m.dispatched[id] = true
	}
	return nil
}

type emptyOutbox struct{}

func (emptyOutbox) Claim(context.Context, pgx.Tx, int) ([]jobs.Event, error) { return nil, nil }
func (emptyOutbox) MarkDispatched(context.Context, pgx.Tx, []int64) error    { return nil }
