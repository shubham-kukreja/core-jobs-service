package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/jobs/internal/db"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
)

// MaxPreview caps GET /v1/schedules/{id}/preview.
const MaxPreview = 50

var (
	scheduleNameRE      = regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{0,62}$`)
	errScheduleNotFound = apperr.New(apperr.NotFound, "schedule not found")
)

// ScheduleStore implements Schedules.
type ScheduleStore struct {
	pool  Pool
	reg   *Registry
	types *ScheduleTypes
	enq   Enqueuer
	clock clock.Clock
}

var _ Schedules = (*ScheduleStore)(nil)

// NewScheduleStore builds the schedule repository.
func NewScheduleStore(pool Pool, reg *Registry, types *ScheduleTypes, enq Enqueuer, clk clock.Clock) *ScheduleStore {
	return &ScheduleStore{pool: pool, reg: reg, types: types, enq: enq, clock: clk}
}

// List returns every schedule by name.
func (s *ScheduleStore) List(ctx context.Context) ([]Schedule, error) {
	rows, err := db.New(s.pool).ListSchedules(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Schedule, len(rows))
	for i, r := range rows {
		out[i] = toSchedule(r)
	}
	return out, nil
}

// Get returns one schedule.
func (s *ScheduleStore) Get(ctx context.Context, id uuid.UUID) (Schedule, error) {
	r, err := db.New(s.pool).GetScheduleByUUID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Schedule{}, errScheduleNotFound
	}
	if err != nil {
		return Schedule{}, err
	}
	return toSchedule(r), nil
}

// Save creates (zero UUID) or replaces a schedule. The next run is
// recomputed from now, so editing never fires missed slots.
func (s *ScheduleStore) Save(ctx context.Context, tx pgx.Tx, in Schedule, actorID int64) (Schedule, error) {
	in = withScheduleDefaults(in)
	now := s.clock.Now()
	next, err := s.validate(in, now)
	if err != nil {
		return Schedule{}, err
	}
	jitterSecs := int32(in.JitterSecs) //nolint:gosec // validated to 0..3600.
	q := db.New(tx)
	var row db.JobsJobSchedule
	if in.UUID == uuid.Nil {
		row, err = q.InsertSchedule(ctx, db.InsertScheduleParams{
			Name: in.Name, Kind: in.Kind, Args: in.Args, ScheduleType: in.ScheduleType, Expr: in.Expr,
			Timezone: in.Timezone, OverlapPolicy: in.OverlapPolicy, CatchupPolicy: in.CatchupPolicy,
			JitterSecs: jitterSecs, Enabled: in.Enabled, NextRunAt: next, CreatedBy: &actorID, Now: now,
		})
	} else {
		row, err = q.UpdateSchedule(ctx, db.UpdateScheduleParams{
			Uuid: in.UUID, Name: in.Name, Kind: in.Kind, Args: in.Args, ScheduleType: in.ScheduleType, Expr: in.Expr,
			Timezone: in.Timezone, OverlapPolicy: in.OverlapPolicy, CatchupPolicy: in.CatchupPolicy,
			JitterSecs: jitterSecs, Enabled: in.Enabled, NextRunAt: next, Now: now,
		})
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Schedule{}, errScheduleNotFound
	case database.IsCode(err, database.CodeUniqueViolation):
		return Schedule{}, apperr.New(apperr.Conflict, fmt.Sprintf("a schedule named %q already exists", in.Name))
	case err != nil:
		return Schedule{}, err
	}
	return toSchedule(row), nil
}

// SetEnabled pauses or resumes a schedule. Resuming restarts from now rather
// than catching up on slots missed while paused.
func (s *ScheduleStore) SetEnabled(ctx context.Context, tx pgx.Tx, id uuid.UUID, enabled bool) error {
	q := db.New(tx)
	row, err := q.GetScheduleByUUIDForUpdate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return errScheduleNotFound
	}
	if err != nil {
		return err
	}
	now := s.clock.Now()
	var next *time.Time
	if enabled && !row.Enabled {
		n, err := s.nextAfter(toSchedule(row), now)
		if err != nil {
			return err
		}
		next = &n
	}
	_, err = q.SetScheduleEnabled(ctx, db.SetScheduleEnabledParams{Uuid: id, Enabled: enabled, NextRunAt: next, Now: now})
	return err
}

// RunNow enqueues the schedule's job immediately, outside its calendar.
// The caller records actorID in its audit entry.
func (s *ScheduleStore) RunNow(ctx context.Context, tx pgx.Tx, id uuid.UUID, _ int64) (RunRef, error) {
	row, err := db.New(tx).GetScheduleByUUID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunRef{}, errScheduleNotFound
	}
	if err != nil {
		return RunRef{}, err
	}
	loc, err := time.LoadLocation(row.Timezone)
	if err != nil {
		return RunRef{}, err
	}
	args, err := ResolveArgTemplates(row.Args, s.clock.Now(), loc)
	if err != nil {
		return RunRef{}, apperr.Wrap(err, apperr.ValidationFailed, "schedule args are invalid")
	}
	return s.enq.EnqueueTx(ctx, tx, RawArgs{KindName: row.Kind, JSON: args},
		WithTrigger(TriggerSchedule), WithSchedule(row.ID))
}

// Preview returns the next n fire times from now (1 ≤ n ≤ MaxPreview).
func (s *ScheduleStore) Preview(ctx context.Context, id uuid.UUID, n int) ([]time.Time, error) {
	sch, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	n = min(max(n, 1), MaxPreview)
	out := make([]time.Time, 0, n)
	t := s.clock.Now()
	for range n {
		if t, err = s.nextAfter(sch, t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func (s *ScheduleStore) nextAfter(sch Schedule, after time.Time) (time.Time, error) {
	typ, ok := s.types.Get(sch.ScheduleType)
	if !ok {
		return time.Time{}, fmt.Errorf("jobs: unknown schedule type %q", sch.ScheduleType)
	}
	loc, err := time.LoadLocation(sch.Timezone)
	if err != nil {
		return time.Time{}, err
	}
	return typ.Next(sch.Expr, after, loc)
}

func withScheduleDefaults(s Schedule) Schedule {
	if s.Timezone == "" {
		s.Timezone = "Asia/Kolkata"
	}
	if s.OverlapPolicy == "" {
		s.OverlapPolicy = OverlapSkip
	}
	if s.CatchupPolicy == "" {
		s.CatchupPolicy = CatchupLatest
	}
	if len(s.Args) == 0 {
		s.Args = json.RawMessage("{}")
	}
	return s
}

// validate checks every field and returns the first fire time. Args are
// checked against the kind after filling templates for today, so a bad
// schedule fails at save time rather than when it fires.
func (s *ScheduleStore) validate(in Schedule, now time.Time) (time.Time, error) {
	var details []apperr.Detail
	bad := func(field, reason string) { details = append(details, apperr.Detail{Field: field, Reason: reason}) }

	if !scheduleNameRE.MatchString(in.Name) {
		bad("name", "invalid_format")
	}
	k, kindOK := s.reg.Get(in.Kind)
	switch {
	case !kindOK:
		bad("kind", "unknown_kind")
	case !k.Spec.Allows(TriggerSchedule):
		bad("kind", "not_schedulable")
	}
	typ, typOK := s.types.Get(in.ScheduleType)
	if !typOK {
		bad("schedule_type", "unknown_type")
	}
	loc, err := time.LoadLocation(in.Timezone)
	if err != nil {
		bad("timezone", "unknown_timezone")
	}
	if typOK && loc != nil {
		if err := typ.Validate(in.Expr, loc); err != nil {
			bad("expr", "invalid_expression")
		}
	}
	switch in.OverlapPolicy {
	case OverlapSkip, OverlapQueue, OverlapCancelPrevious:
	default:
		bad("overlap_policy", "invalid_value")
	}
	switch in.CatchupPolicy {
	case CatchupNone, CatchupLatest, CatchupAll:
	default:
		bad("catchup_policy", "invalid_value")
	}
	if in.JitterSecs < 0 || in.JitterSecs > 3600 {
		bad("jitter_secs", "out_of_range")
	}
	if kindOK && loc != nil {
		resolved, err := ResolveArgTemplates(in.Args, now, loc)
		if err != nil {
			bad("args", "invalid_template")
		} else if err := k.ValidateArgs(resolved); err != nil {
			bad("args", "invalid_args")
		}
	}
	if len(details) > 0 {
		return time.Time{}, apperr.New(apperr.ValidationFailed, "schedule is invalid", details...)
	}
	return typ.Next(in.Expr, now, loc)
}

func toSchedule(r db.JobsJobSchedule) Schedule {
	return Schedule{
		ID: r.ID, UUID: r.Uuid, Name: r.Name, Kind: r.Kind, Args: r.Args, ScheduleType: r.ScheduleType,
		Expr: r.Expr, Timezone: r.Timezone, OverlapPolicy: r.OverlapPolicy, CatchupPolicy: r.CatchupPolicy,
		JitterSecs: int(r.JitterSecs), Enabled: r.Enabled, NextRunAt: r.NextRunAt, LastRunID: r.LastRunID,
	}
}
