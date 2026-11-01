// Package jobs is the job system: kinds live in code (registered handlers),
// triggers live in data (schedules, outbox events, users, chains). Only
// internal/jobs/river imports the queue engine.
//
// jobs depends on platform only. Handlers receive their dependencies through
// the kind's constructor, not through RunCtx.
package jobs

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
)

// Args is a job's JSON-serialisable arguments. Add fields, never rename.
type Args interface{ Kind() string }

// TriggerType is what started a job.
type TriggerType string

const (
	TriggerSchedule TriggerType = "schedule"
	TriggerEvent    TriggerType = "event"
	TriggerUser     TriggerType = "user"
	TriggerChain    TriggerType = "chain"
	TriggerSystem   TriggerType = "system"
)

// Queues.
const (
	QueueIngest   = "ingest"
	QueueCompute  = "compute"
	QueueModel    = "model"
	QueueBacktest = "backtest"
	QueueSystem   = "system"
)

// Status is a job run's lifecycle state.
type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
	StatusSkipped   Status = "skipped"
)

// UniqueOpts deduplicates enqueues.
type UniqueOpts struct {
	ByArgs []string      // arg fields that form the key; empty = all
	Period time.Duration // dedupe window
	States []Status      // default: queued, running
}

// Spec is a kind's static definition.
type Spec struct {
	Kind            string
	Queue           string
	Triggers        []TriggerType
	UserTriggerable bool
	Roles           []string // roles allowed to start it via the API
	MaxAttempts     int
	Timeout         time.Duration
	Backoff         func(attempt int) time.Duration // nil = engine default
	Unique          *UniqueOpts
	Concurrency     int // global cap; 0 = queue default
	MaxPending      int // queued + running cap for user submits; 0 = none (backtest.run: 3)
	ArgsVersion     int
	ArgsSchema      json.RawMessage // JSON Schema served by /v1/job-kinds; generated if nil
	ResultTTL       time.Duration
}

// Allows reports whether t may start this kind.
func (s Spec) Allows(t TriggerType) bool {
	for _, x := range s.Triggers {
		if x == t {
			return true
		}
	}
	return false
}

// Outcome is a handler's result.
type Outcome struct {
	Summary map[string]any // stored in job_runs.result; must not contain secrets
	BlobURI string         // large result
	Next    []Args         // chained follow-ups (trigger = chain), enqueued in the completion tx
}

// RunCtx is per-execution context passed to handlers.
type RunCtx struct {
	RunID     int64
	RunUUID   uuid.UUID
	RootID    int64
	Attempt   int
	Trigger   TriggerType
	UserID    *int64
	Log       *slog.Logger
	Progress  func(pct int, msg string) // throttled writes to job_runs
	Cancelled func() bool               // also reflected in ctx.Done()
}

// Handler executes one kind.
type Handler[A Args] interface {
	Handle(ctx context.Context, rc *RunCtx, args A) (Outcome, error)
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc[A Args] func(ctx context.Context, rc *RunCtx, args A) (Outcome, error)

func (f HandlerFunc[A]) Handle(ctx context.Context, rc *RunCtx, a A) (Outcome, error) {
	return f(ctx, rc, a)
}

// RunRef identifies an enqueued run.
type RunRef struct {
	ID     int64
	UUID   uuid.UUID
	Status Status
	// Duplicate is true when uniqueness or an idempotency key matched an
	// existing run; ID then refers to that run.
	Duplicate bool
}

// EnqueueOpts are set through Opt functions.
type EnqueueOpts struct {
	Trigger        TriggerType
	UserID         *int64
	ParentID       *int64
	RootID         *int64
	ScheduleID     *int64
	IdempotencyKey string
	RunAt          time.Time
	Priority       int
}

// Opt configures an enqueue.
type Opt func(*EnqueueOpts)

func WithTrigger(t TriggerType) Opt   { return func(o *EnqueueOpts) { o.Trigger = t } }
func WithUser(id int64) Opt           { return func(o *EnqueueOpts) { o.UserID = &id } }
func WithSchedule(id int64) Opt       { return func(o *EnqueueOpts) { o.ScheduleID = &id } }
func WithIdempotencyKey(k string) Opt { return func(o *EnqueueOpts) { o.IdempotencyKey = k } }
func WithRunAt(t time.Time) Opt       { return func(o *EnqueueOpts) { o.RunAt = t } }
func WithPriority(p int) Opt          { return func(o *EnqueueOpts) { o.Priority = p } }
func WithParent(parent, root int64) Opt {
	return func(o *EnqueueOpts) { o.ParentID, o.RootID = &parent, &root }
}

// Enqueuer starts jobs. EnqueueTx commits the job with the caller's
// transaction (transactional enqueue).
type Enqueuer interface {
	Enqueue(ctx context.Context, a Args, o ...Opt) (RunRef, error)
	EnqueueTx(ctx context.Context, tx pgx.Tx, a Args, o ...Opt) (RunRef, error)
}

// Event is an outbox event delivered to subscribers.
type Event struct {
	ID      int64
	Topic   string
	Payload json.RawMessage
}

// Subscriber maps an event to follow-up jobs (trigger = event).
type Subscriber interface {
	Topic() string
	Handle(ctx context.Context, ev Event) ([]Args, error)
}

// ScheduleType computes the next fire time for an expression. Registered by
// name ("cron", "interval", "calendar") so new calendars need no scheduler change.
type ScheduleType interface {
	Name() string
	Validate(expr string, loc *time.Location) error
	Next(expr string, after time.Time, loc *time.Location) (time.Time, error)
}

// JobRun is the public view of one execution.
type JobRun struct {
	ID              int64
	UUID            uuid.UUID
	Kind            string
	Args            json.RawMessage
	ArgsVersion     int
	Trigger         TriggerType
	ScheduleID      *int64
	UserID          *int64
	ParentID        *int64
	RootID          *int64
	Queue           string
	Status          Status
	Attempt         int
	Progress        int
	Message         string
	Result          json.RawMessage
	ResultURI       string
	Error           *RunError
	CancelRequested bool
	QueuedAt        time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
}

// RunError is the stored failure. Message is safe to show users.
type RunError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// RunFilter narrows List.
type RunFilter struct {
	Kind    string
	Status  Status
	Trigger TriggerType
	RootID  *int64
	Since   *time.Time
	Page    paging.Request
}

// RunTree is a run and everything it caused.
type RunTree struct {
	JobRun
	Children []RunTree
}

// Runs is the read/control surface used by the API.
type Runs interface {
	Get(ctx context.Context, id uuid.UUID) (JobRun, error)
	List(ctx context.Context, f RunFilter) (paging.Page[JobRun], error)
	Tree(ctx context.Context, id uuid.UUID, maxDepth int) (RunTree, error)
	RequestCancel(ctx context.Context, tx pgx.Tx, id uuid.UUID) error
	Retry(ctx context.Context, tx pgx.Tx, id uuid.UUID, actorID int64) (RunRef, error)
	// Watch streams progress and status changes until the run finishes or ctx ends.
	Watch(ctx context.Context, id uuid.UUID) (<-chan RunUpdate, error)
}

// RunUpdate is one SSE event.
type RunUpdate struct {
	Kind     string // progress | status
	Progress int
	Message  string
	Status   Status
	Result   json.RawMessage
}

// Schedule is a stored schedule row.
type Schedule struct {
	ID            int64
	UUID          uuid.UUID
	Name          string
	Kind          string
	Args          json.RawMessage
	ScheduleType  string
	Expr          string
	Timezone      string
	OverlapPolicy string // skip | queue | cancel_previous
	CatchupPolicy string // none | latest | all
	JitterSecs    int
	Enabled       bool
	NextRunAt     time.Time
	LastRunID     *int64
}

// Schedules manages schedule rows; the scheduler.tick job fires them.
type Schedules interface {
	List(ctx context.Context) ([]Schedule, error)
	Get(ctx context.Context, id uuid.UUID) (Schedule, error)
	Save(ctx context.Context, tx pgx.Tx, s Schedule, actorID int64) (Schedule, error)
	SetEnabled(ctx context.Context, tx pgx.Tx, id uuid.UUID, enabled bool) error
	RunNow(ctx context.Context, tx pgx.Tx, id uuid.UUID, actorID int64) (RunRef, error)
	Preview(ctx context.Context, id uuid.UUID, n int) ([]time.Time, error)
}
