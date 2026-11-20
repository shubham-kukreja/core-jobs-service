// Package app is the composition root: it builds every module from its
// constructor and connects them through their contracts. Nothing else in the
// codebase constructs modules, so the dependency graph lives in one place.
package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"cloud.google.com/go/civil"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shubham-kukreja/core-jobs-service/internal/audit"
	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/derive"
	"github.com/shubham-kukreja/core-jobs-service/internal/ingest"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	jriver "github.com/shubham-kukreja/core-jobs-service/internal/jobs/river"
	"github.com/shubham-kukreja/core-jobs-service/internal/model"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/blob"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/config"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/telemetry"
	"github.com/shubham-kukreja/core-jobs-service/internal/quality"
	"github.com/shubham-kukreja/core-jobs-service/internal/seed"
)

// Core holds the modules shared by the api and worker processes.
type Core struct {
	Pool          *pgxpool.Pool
	Clock         clock.Clock
	Log           *slog.Logger
	Catalog       *catalog.Store
	Datapoints    *datapoints.Store
	Primitives    derive.Registry
	Cascade       derive.Cascade
	Explainer     derive.Explainer
	Model         *model.Store
	Engine        *model.Pipeline
	Loader        model.InputLoader
	Quality       quality.Checker
	Uploads       ingest.Uploads  // nil until EnableIngest
	Mappings      ingest.Mappings // nil until EnableIngest
	Registry      *jobs.Registry
	ScheduleTypes *jobs.ScheduleTypes
	Jobs          *jobs.Service
	Runs          *jobs.RunStore
	Schedules     *jobs.ScheduleStore
	Metrics       *telemetry.Prometheus

	registerOnce sync.Once
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
	prims := derive.NewDefaultRegistry()
	cat := catalog.New(pool, prims)
	dps := datapoints.New(pool, cat, clk)
	eng := model.NewEngine()
	ms := model.NewStore(pool, model.NewValidator(eng, cat))
	types.Register(jobs.CalendarType{Holidays: NSEHolidays, Reviews: reviewCalendar{ms}.dates})
	loader := model.NewLoader(dps, ms, clk)
	mdeps := model.JobDeps{DB: pool, Store: ms, Engine: eng, Loader: loader, Reader: dps, Writer: dps, Clock: clk}
	model.RegisterRun(reg, mdeps)
	model.RegisterVeto(reg, mdeps)
	casc := derive.NewCascade(derive.CascadeDeps{
		DB: pool, Catalog: cat, Reader: dps, Writer: dps, Batches: dps,
		Registry: prims, Clock: clk, Governance: DefaultGovernance,
	})
	derive.RegisterCascade(reg, casc)
	prom := telemetry.NewPrometheus()
	prom.RegisterPool(pool)
	qc := quality.NewChecker(cat, dps, DefaultGovernance.PercentileMinObservations)
	quality.RegisterCheck(reg, qc, clk)
	return &Core{
		Pool:       pool,
		Clock:      clk,
		Log:        log,
		Catalog:    cat,
		Datapoints: dps,
		Primitives: prims,
		Cascade:    casc,
		Model:      ms,
		Engine:     eng,
		Loader:     loader,
		Explainer: derive.NewExplainer(derive.ExplainerDeps{
			Catalog: cat, Reader: dps, Registry: prims, Clock: clk, Governance: DefaultGovernance,
		}),
		Quality:       qc,
		Registry:      reg,
		ScheduleTypes: types,
		Jobs:          svc,
		Runs:          jobs.NewRunStore(pool, svc, clk),
		Schedules:     jobs.NewScheduleStore(pool, reg, types, svc, clk),
		Metrics:       prom,
	}, nil
}

// NewWorker builds the queue worker: job executor, scheduler and outbox
// dispatcher. The derive.cascade and veto.evaluate subscribers are always
// installed; subs adds more.
func (c *Core) NewWorker(subs ...jobs.Subscriber) (*jriver.Worker, error) {
	exec := jobs.NewExecutor(c.Pool, c.Registry, c.Jobs, jobs.ExecutorConfig{Log: c.Log, Clock: c.Clock, Metrics: c.Metrics})
	c.registerBacklogs()
	sched := jobs.NewScheduler(c.Pool, c.ScheduleTypes, c.Jobs, c.Clock, c.Log)
	subs = append([]jobs.Subscriber{derive.Subscriber{}, model.VetoSubscriber{}}, subs...)
	disp := jobs.NewDispatcher(c.Pool, outboxSource{c.Datapoints}, jobs.NewSubscribers(subs...), c.Jobs, c.Log)
	return jriver.NewWorker(c.Pool, jriver.WorkerDeps{
		Registry:   c.Registry,
		Executor:   exec,
		Scheduler:  sched,
		Dispatcher: disp,
		Log:        c.Log,
	})
}

// registerBacklogs exports queue depth and outbox lag. Only the worker
// registers them, so a fleet of API processes does not multiply the series.
func (c *Core) registerBacklogs() {
	c.registerOnce.Do(func() {
		c.Metrics.RegisterBacklog("jobs", "Open job runs by kind and status.", []string{"kind", "status"},
			func(ctx context.Context) ([]telemetry.Backlog, error) {
				rows, err := c.Runs.Backlog(ctx)
				if err != nil {
					return nil, err
				}
				now := c.Clock.Now()
				out := make([]telemetry.Backlog, len(rows))
				for i, r := range rows {
					out[i] = telemetry.Backlog{Labels: []string{r.Kind, string(r.Status)}, Count: float64(r.Count), OldestAge: now.Sub(r.Oldest)}
				}
				return out, nil
			}, c.Log)
		c.Metrics.RegisterBacklog("outbox", "Undispatched datapoint events.", nil,
			func(ctx context.Context) ([]telemetry.Backlog, error) {
				n, oldest, err := c.Datapoints.Pending(ctx)
				if err != nil {
					return nil, err
				}
				b := telemetry.Backlog{Count: float64(n)}
				if n > 0 {
					b.OldestAge = c.Clock.Now().Sub(oldest)
				}
				return []telemetry.Backlog{b}, nil
			}, c.Log)
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

// reviewCalendar reads a review cycle's dates from the default model's
// active config, for review:<cycle> calendar schedules. The scheduler's
// calendar interface passes no context, so each lookup gets a short
// deadline of its own.
type reviewCalendar struct{ ms *model.Store }

func (r reviewCalendar) dates(cycle string) []civil.Date {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cv, err := r.ms.Active(ctx, model.DefaultModel)
	if err != nil {
		return nil
	}
	return cv.Config.ReviewCalendar[cycle]
}

// NSEHolidays is the exchange holiday list for calendar schedules. It is
// empty until the owner supplies the official NSE circular's dates (open
// question in HANDOFF.md); until then only weekends are skipped.
var NSEHolidays = jobs.HolidaySet{}

// DefaultGovernance holds the percentile defaults until the model config
// supplies them (Phase 3).
var DefaultGovernance = derive.Governance{
	PercentileWindowYears:     15,
	PercentileMinObservations: 24,
	RespectDefinitionBreaks:   true,
}

// EnableIngest builds uploads and import mappings on blobs and registers
// import.file. The api (to enqueue) and the worker (to run) both call it.
func (c *Core) EnableIngest(blobs blob.Store, maxUploadBytes int64) {
	c.Uploads = ingest.NewUploads(c.Pool, blobs, c.Clock, maxUploadBytes)
	c.Mappings = ingest.NewMappings(c.Pool, c.Catalog)
	ingest.RegisterImport(c.Registry, &ingest.Importer{
		DB: c.Pool, Uploads: c.Uploads, Mappings: c.Mappings, Parser: ingest.NewParser(),
		Writer: c.Datapoints, Clock: c.Clock, Limits: ingest.DefaultLimits,
	})
}

// NewBlobStore opens the configured S3-compatible store and checks it is
// reachable, private and (outside development) encrypted.
func NewBlobStore(ctx context.Context, cfg config.Config) (blob.Store, error) {
	s, err := blob.NewS3(blob.S3Options{
		Endpoint: cfg.BlobEndpoint, Bucket: cfg.BlobBucket, AccessKey: cfg.BlobAccessKey.Reveal(),
		SecretKey: cfg.BlobSecretKey.Reveal(), UseTLS: cfg.BlobUseTLS, ServerSideEncryption: cfg.BlobSSE,
	})
	if err != nil {
		return nil, err
	}
	if err := s.Verify(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// SeedTarget is where `app seed apply` writes: the catalog, import mappings,
// model configs, risk profiles, portfolios, schedules, the audit log, and
// derive.cascade rebuilds for new or changed derivations.
func (c *Core) SeedTarget() seed.Target {
	maps := c.Mappings
	if maps == nil {
		maps = ingest.NewMappings(c.Pool, c.Catalog)
	}
	return seed.Target{DB: c.Pool, Catalog: c.Catalog, Mappings: maps, Audit: audit.NewPG(c.Pool), Rebuilds: rebuilds{c.Jobs},
		Model: c.Model, Schedules: c.Schedules}
}
