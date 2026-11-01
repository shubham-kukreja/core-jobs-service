package jobs

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/jobs/internal/db"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
)

// Overlap and catch-up policies (job_schedules CHECK constraints).
const (
	OverlapSkip           = "skip"
	OverlapQueue          = "queue"
	OverlapCancelPrevious = "cancel_previous"

	CatchupNone   = "none"
	CatchupLatest = "latest"
	CatchupAll    = "all"
)

const (
	// MaxCatchup caps how many missed slots catch-up "all" fires at once.
	MaxCatchup = 100
	// catchupGrace is how late a slot may fire under catch-up "none".
	catchupGrace = 5 * time.Minute
	// maxSlotScan bounds slot iteration for very frequent schedules.
	maxSlotScan = 10_000
	tickBatch   = 50
)

// Scheduler fires due schedules. Tick runs every minute on one worker
// (River periodic job, leader only); concurrent ticks are still safe because
// due rows are locked FOR UPDATE SKIP LOCKED and each slot carries an
// idempotency key.
type Scheduler struct {
	pool  Pool
	types *ScheduleTypes
	enq   Enqueuer
	clock clock.Clock
	log   *slog.Logger
}

// NewScheduler builds the scheduler.
func NewScheduler(pool Pool, types *ScheduleTypes, enq Enqueuer, clk clock.Clock, log *slog.Logger) *Scheduler {
	return &Scheduler{pool: pool, types: types, enq: enq, clock: clk, log: log}
}

// Tick fires every due slot and returns how many runs were enqueued.
func (s *Scheduler) Tick(ctx context.Context) (int, error) {
	total := 0
	for {
		fired, rows, err := s.tickBatch(ctx)
		total += fired
		if err != nil || rows < tickBatch {
			return total, err
		}
	}
}

func (s *Scheduler) tickBatch(ctx context.Context) (fired, rows int, err error) {
	now := s.clock.Now()
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		due, err := db.New(tx).DueSchedules(ctx, db.DueSchedulesParams{Now: now, Lim: tickBatch})
		if err != nil {
			return err
		}
		rows = len(due)
		for _, row := range due {
			// A savepoint per schedule: one broken schedule never blocks the rest.
			sp, err := tx.Begin(ctx)
			if err != nil {
				return err
			}
			n, ferr := s.fire(ctx, sp, row, now)
			if ferr != nil {
				_ = sp.Rollback(ctx)
				s.log.Error("schedule fire failed", "schedule", row.Name, "err", ferr)
				if err := s.backoff(ctx, tx, row, now); err != nil {
					return err
				}
				continue
			}
			if err := sp.Commit(ctx); err != nil {
				return err
			}
			fired += n
		}
		return nil
	})
	return fired, rows, err
}

// fire enqueues the slots a schedule owes and advances next_run_at.
func (s *Scheduler) fire(ctx context.Context, tx pgx.Tx, row db.JobsJobSchedule, now time.Time) (int, error) {
	typ, ok := s.types.Get(row.ScheduleType)
	if !ok {
		return 0, fmt.Errorf("unknown schedule type %q", row.ScheduleType)
	}
	loc, err := time.LoadLocation(row.Timezone)
	if err != nil {
		return 0, fmt.Errorf("timezone: %w", err)
	}
	due, next, err := dueSlots(typ, row.Expr, row.NextRunAt, now, loc, MaxCatchup)
	if err != nil {
		return 0, err
	}
	slots := applyCatchup(row.CatchupPolicy, due, now)

	q := db.New(tx)
	if len(slots) > 0 && row.OverlapPolicy != OverlapQueue {
		active, err := q.ActiveRunsForSchedule(ctx, &row.ID)
		if err != nil {
			return 0, err
		}
		switch {
		case len(active) == 0:
		case row.OverlapPolicy == OverlapSkip:
			s.log.Info("schedule skipped: previous run still active", "schedule", row.Name, "slots", len(slots))
			slots = nil
		case row.OverlapPolicy == OverlapCancelPrevious:
			for _, id := range active {
				if _, err := q.RequestCancel(ctx, db.RequestCancelParams{ID: id, Now: &now}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return 0, err
				}
			}
		}
	}

	var lastRun *int64
	for _, slot := range slots {
		ref, err := s.enqueueSlot(ctx, tx, row, slot, loc)
		if err != nil {
			return 0, err
		}
		lastRun = &ref.ID
	}
	if err := q.AdvanceSchedule(ctx, db.AdvanceScheduleParams{ID: row.ID, NextRunAt: next, LastRunID: lastRun}); err != nil {
		return 0, err
	}
	return len(slots), nil
}

func (s *Scheduler) enqueueSlot(ctx context.Context, tx pgx.Tx, row db.JobsJobSchedule, slot time.Time, loc *time.Location) (RunRef, error) {
	args, err := ResolveArgTemplates(row.Args, slot, loc)
	if err != nil {
		return RunRef{}, err
	}
	return s.enq.EnqueueTx(ctx, tx, RawArgs{KindName: row.Kind, JSON: args},
		WithTrigger(TriggerSchedule),
		WithSchedule(row.ID),
		WithRunAt(slot.Add(jitter(row.ID, slot, int(row.JitterSecs)))),
		// Belt and braces with the row lock: a slot can never fire twice.
		WithIdempotencyKey(fmt.Sprintf("schedule:%d:%d", row.ID, slot.Unix())),
	)
}

// backoff pushes a failing schedule an hour out so it does not hot-loop
// every tick; the error is logged for operators.
func (s *Scheduler) backoff(ctx context.Context, tx pgx.Tx, row db.JobsJobSchedule, now time.Time) error {
	return db.New(tx).AdvanceSchedule(ctx, db.AdvanceScheduleParams{ID: row.ID, NextRunAt: now.Add(time.Hour)})
}

// dueSlots returns the slots at or before now (at most limit, most recent
// kept), starting from first, and the first slot after now.
func dueSlots(typ ScheduleType, expr string, first, now time.Time, loc *time.Location, limit int) (due []time.Time, next time.Time, err error) {
	slot := first
	for range maxSlotScan {
		if slot.After(now) {
			return due, slot, nil
		}
		due = append(due, slot)
		if len(due) > limit {
			due = due[1:]
		}
		if slot, err = typ.Next(expr, slot, loc); err != nil {
			return nil, time.Time{}, err
		}
	}
	// Far behind a very frequent schedule: resume from now.
	next, err = typ.Next(expr, now, loc)
	return due, next, err
}

// applyCatchup picks which due slots to fire.
func applyCatchup(policy string, due []time.Time, now time.Time) []time.Time {
	if len(due) == 0 {
		return nil
	}
	last := due[len(due)-1]
	switch policy {
	case CatchupAll:
		return due
	case CatchupNone:
		if now.Sub(last) <= catchupGrace {
			return []time.Time{last}
		}
		return nil
	default: // latest
		return []time.Time{last}
	}
}

// jitter spreads a slot by a deterministic offset in [0, maxSecs] seconds.
func jitter(scheduleID int64, slot time.Time, maxSecs int) time.Duration {
	if maxSecs <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d:%d", scheduleID, slot.Unix())
	return time.Duration(h.Sum64()%uint64(maxSecs+1)) * time.Second //nolint:gosec // maxSecs ≤ 3600.
}
