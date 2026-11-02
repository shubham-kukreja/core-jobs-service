// Package river adapts the River queue engine to the jobs module. It is the
// only package that imports River: swapping engines means replacing this
// package and nothing else.
//
// River stores one job kind, core.run, whose args point at our job_runs row.
// Kinds, args, uniqueness, idempotency and bookkeeping all live in jobs; River
// provides durable delivery, retries with backoff, snoozing and leader-only
// periodic jobs (scheduler.tick, outbox.dispatch).
package river

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/migrate"
)

// River job kinds. These never appear in job_runs.
const (
	kindRun      = "core.run"
	kindTick     = "core.scheduler_tick"
	kindDispatch = "core.outbox_dispatch"
)

// Intervals of the infrastructure jobs.
const (
	TickInterval     = time.Minute
	DispatchInterval = 5 * time.Second
)

const defaultQueueWorkers = 5

// runArgs points River at one job_runs row. Kind is carried only so retry
// backoff can use the kind's Spec without a database read.
type runArgs struct {
	RunID   int64  `json:"run_id"`
	JobKind string `json:"kind"`
}

func (runArgs) Kind() string { return kindRun }

type tickArgs struct{}

func (tickArgs) Kind() string { return kindTick }

type dispatchArgs struct{}

func (dispatchArgs) Kind() string { return kindDispatch }

// Engine implements jobs.Engine with an insert-only River client. The api
// and worker processes both use it to enqueue.
type Engine struct {
	client *river.Client[pgx.Tx]
}

var _ jobs.Engine = (*Engine)(nil)

// NewEngine builds an insert-only River client.
func NewEngine(pool *pgxpool.Pool, log *slog.Logger) (*Engine, error) {
	c, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: migrate.RiverSchema, Logger: log})
	if err != nil {
		return nil, fmt.Errorf("river: insert client: %w", err)
	}
	return &Engine{client: c}, nil
}

// InsertTx inserts the River job for a run in the caller's transaction.
func (e *Engine) InsertTx(ctx context.Context, tx pgx.Tx, j jobs.EngineJob) (int64, error) {
	res, err := e.client.InsertTx(ctx, tx, runArgs{RunID: j.RunID, JobKind: j.Kind}, &river.InsertOpts{
		Queue:       j.Queue,
		MaxAttempts: j.MaxAttempts,
		Priority:    j.Priority,
		ScheduledAt: j.RunAt,
	})
	if err != nil {
		return 0, err
	}
	return res.Job.ID, nil
}

// WorkerDeps are what the working client runs.
type WorkerDeps struct {
	Registry   *jobs.Registry
	Executor   *jobs.Executor
	Scheduler  *jobs.Scheduler
	Dispatcher *jobs.Dispatcher
	Log        *slog.Logger
	// QueueWorkers overrides per-queue parallelism of this process.
	QueueWorkers map[string]int
}

// Worker is the working River client of the worker process.
type Worker struct {
	client *river.Client[pgx.Tx]
}

// NewWorker builds the working client: one queue per kind queue, the core.run
// worker, and the leader-only periodic infrastructure jobs.
func NewWorker(pool *pgxpool.Pool, d WorkerDeps) (*Worker, error) {
	if d.Registry == nil || d.Executor == nil || d.Scheduler == nil || d.Dispatcher == nil || d.Log == nil {
		return nil, errors.New("river: incomplete worker dependencies")
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, &runWorker{exec: d.Executor, reg: d.Registry})
	river.AddWorker(workers, &tickWorker{s: d.Scheduler, log: d.Log})
	river.AddWorker(workers, &dispatchWorker{d: d.Dispatcher, log: d.Log})

	c, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Schema:  migrate.RiverSchema,
		Logger:  d.Log,
		Queues:  queues(d.Registry, d.QueueWorkers),
		Workers: workers,
		PeriodicJobs: []*river.PeriodicJob{
			periodic("scheduler.tick", TickInterval, tickArgs{}),
			periodic("outbox.dispatch", DispatchInterval, dispatchArgs{}),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("river: worker client: %w", err)
	}
	return &Worker{client: c}, nil
}

// Start begins working jobs; it returns once the client is running.
func (w *Worker) Start(ctx context.Context) error { return w.client.Start(ctx) }

// Stop waits for running jobs to finish (bounded by ctx); interrupted runs
// are put back without spending an attempt.
func (w *Worker) Stop(ctx context.Context) error { return w.client.Stop(ctx) }

func periodic(id string, every time.Duration, args river.JobArgs) *river.PeriodicJob {
	return river.NewPeriodicJob(river.PeriodicInterval(every),
		func() (river.JobArgs, *river.InsertOpts) {
			return args, &river.InsertOpts{
				Queue:       jobs.QueueSystem,
				MaxAttempts: 1,
				UniqueOpts:  river.UniqueOpts{ByPeriod: every},
			}
		},
		&river.PeriodicJobOpts{ID: id, RunOnStart: true})
}

// queues gives every queue used by a kind its parallelism: the largest
// Spec.Concurrency of its kinds (default 5), overridable per process.
func queues(reg *jobs.Registry, override map[string]int) map[string]river.QueueConfig {
	n := map[string]int{jobs.QueueSystem: 2}
	for _, k := range reg.All() {
		c := k.Spec.Concurrency
		if c <= 0 {
			c = defaultQueueWorkers
		}
		n[k.Spec.Queue] = max(n[k.Spec.Queue], c)
	}
	for q, c := range override {
		if c > 0 {
			n[q] = c
		}
	}
	out := make(map[string]river.QueueConfig, len(n))
	for q, c := range n {
		out[q] = river.QueueConfig{MaxWorkers: c}
	}
	return out
}

// runWorker hands a core.run job to the executor.
type runWorker struct {
	river.WorkerDefaults[runArgs]
	exec *jobs.Executor
	reg  *jobs.Registry
}

// Timeout disables River's timeout: the executor applies Spec.Timeout so the
// run can be recorded as timed out.
func (w *runWorker) Timeout(*river.Job[runArgs]) time.Duration { return -1 }

// NextRetry applies the kind's backoff, if it defines one.
func (w *runWorker) NextRetry(j *river.Job[runArgs]) time.Time {
	if k, ok := w.reg.Get(j.Args.JobKind); ok && k.Spec.Backoff != nil {
		return time.Now().Add(k.Spec.Backoff(j.Attempt))
	}
	return time.Time{}
}

func (w *runWorker) Work(ctx context.Context, j *river.Job[runArgs]) error {
	return toRiverError(w.exec.Execute(ctx, jobs.Execution{
		RunID: j.Args.RunID, Attempt: j.Attempt, MaxAttempts: j.MaxAttempts,
	}))
}

// toRiverError maps executor outcomes onto River's controls.
func toRiverError(err error) error {
	switch {
	case err == nil:
		return nil
	case jobs.IsPermanent(err):
		return river.JobCancel(err)
	}
	if d, ok := jobs.SnoozeDuration(err); ok {
		return river.JobSnooze(d)
	}
	return err
}

type tickWorker struct {
	river.WorkerDefaults[tickArgs]
	s   *jobs.Scheduler
	log *slog.Logger
}

func (w *tickWorker) Timeout(*river.Job[tickArgs]) time.Duration { return TickInterval }

func (w *tickWorker) Work(ctx context.Context, _ *river.Job[tickArgs]) error {
	n, err := w.s.Tick(ctx)
	if n > 0 {
		w.log.Info("scheduler fired", "runs", n)
	}
	return err
}

type dispatchWorker struct {
	river.WorkerDefaults[dispatchArgs]
	d   *jobs.Dispatcher
	log *slog.Logger
}

func (w *dispatchWorker) Timeout(*river.Job[dispatchArgs]) time.Duration { return 30 * time.Second }

func (w *dispatchWorker) Work(ctx context.Context, _ *river.Job[dispatchArgs]) error {
	_, err := w.d.Dispatch(ctx)
	return err
}
