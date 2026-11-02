// Package app is the composition root: it builds every module from its
// constructor and connects them through their contracts. Nothing else in the
// codebase constructs modules, so the dependency graph lives in one place.
package app

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	jriver "github.com/shubham-kukreja/core-jobs-service/internal/jobs/river"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
)

// Core holds the modules shared by the api and worker processes.
type Core struct {
	Pool          *pgxpool.Pool
	Clock         clock.Clock
	Log           *slog.Logger
	Catalog       *catalog.Store
	Datapoints    *datapoints.Store
	Registry      *jobs.Registry
	ScheduleTypes *jobs.ScheduleTypes
	Jobs          *jobs.Service
	Runs          *jobs.RunStore
	Schedules     *jobs.ScheduleStore
}

// NewCore builds the shared modules. Job kinds are registered on
// Core.Registry before a worker is started.
func NewCore(pool *pgxpool.Pool, log *slog.Logger, clk clock.Clock) (*Core, error) {
	engine, err := jriver.NewEngine(pool, log)
	if err != nil {
		return nil, err
	}
	reg := jobs.NewRegistry()
	types := jobs.DefaultScheduleTypes()
	svc := jobs.NewService(pool, reg, engine, clk)
	cat := catalog.New(pool, noDerivations{})
	return &Core{
		Pool:          pool,
		Clock:         clk,
		Log:           log,
		Catalog:       cat,
		Datapoints:    datapoints.New(pool, cat, clk),
		Registry:      reg,
		ScheduleTypes: types,
		Jobs:          svc,
		Runs:          jobs.NewRunStore(pool, svc, clk),
		Schedules:     jobs.NewScheduleStore(pool, reg, types, svc, clk),
	}, nil
}

// NewWorker builds the queue worker: job executor, scheduler and outbox
// dispatcher. subs are the outbox subscribers (derive.cascade, veto.evaluate
// from Phase 2).
func (c *Core) NewWorker(subs ...jobs.Subscriber) (*jriver.Worker, error) {
	exec := jobs.NewExecutor(c.Pool, c.Registry, c.Jobs, jobs.ExecutorConfig{Log: c.Log, Clock: c.Clock})
	sched := jobs.NewScheduler(c.Pool, c.ScheduleTypes, c.Jobs, c.Clock, c.Log)
	disp := jobs.NewDispatcher(c.Pool, outboxSource{c.Datapoints}, jobs.NewSubscribers(subs...), c.Jobs, c.Log)
	return jriver.NewWorker(c.Pool, jriver.WorkerDeps{
		Registry:   c.Registry,
		Executor:   exec,
		Scheduler:  sched,
		Dispatcher: disp,
		Log:        c.Log,
	})
}

// outboxSource adapts the datapoints outbox to the jobs dispatcher, which
// does not import datapoints.
type outboxSource struct{ ob datapoints.Outbox }

func (o outboxSource) Claim(ctx context.Context, tx pgx.Tx, limit int) ([]jobs.Event, error) {
	evs, err := o.ob.Claim(ctx, tx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]jobs.Event, len(evs))
	for i, e := range evs {
		out[i] = jobs.Event{ID: e.ID, Topic: e.Topic, Payload: e.Payload}
	}
	return out, nil
}

func (o outboxSource) MarkDispatched(ctx context.Context, tx pgx.Tx, ids []int64) error {
	return o.ob.MarkDispatched(ctx, tx, ids)
}

// noDerivations rejects derived series until the derive primitive registry
// lands in Phase 2 (P2-T3-01). Raw and judgment series save normally.
type noDerivations struct{}

var errNoDerivations = errors.New("app: derivations are not available until the derive registry is wired")

func (noDerivations) ValidateDerivation(catalog.Derivation) ([]string, error) {
	return nil, apperr.Wrap(errNoDerivations, apperr.NotAvailable, "derived series cannot be saved yet")
}
