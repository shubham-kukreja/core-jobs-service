package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/jobs/internal/db"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
)

// Pool is what the jobs module needs from the database pool.
type Pool interface {
	database.TxBeginner
	database.DBTX
}

// EngineJob is what the queue engine stores: a pointer to our job_runs row
// plus scheduling hints. Args stay in job_runs, never in the engine.
type EngineJob struct {
	RunID       int64
	Kind        string
	Queue       string
	MaxAttempts int
	Priority    int // 1 (highest) … 4
	RunAt       time.Time
}

// Engine is the queue engine port. Only internal/jobs/river implements it.
type Engine interface {
	InsertTx(ctx context.Context, tx pgx.Tx, j EngineJob) (engineJobID int64, err error)
}

// Service implements Enqueuer on top of job_runs and an Engine.
type Service struct {
	pool   Pool
	reg    *Registry
	engine Engine
	clock  clock.Clock
}

// NewService builds the enqueuer.
func NewService(pool Pool, reg *Registry, engine Engine, clk clock.Clock) *Service {
	return &Service{pool: pool, reg: reg, engine: engine, clock: clk}
}

var _ Enqueuer = (*Service)(nil)

// Enqueue starts a job in its own transaction.
func (s *Service) Enqueue(ctx context.Context, a Args, o ...Opt) (RunRef, error) {
	var ref RunRef
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		ref, err = s.EnqueueTx(ctx, tx, a, o...)
		return err
	})
	return ref, err
}

// EnqueueTx validates the args, applies idempotency, uniqueness and pending
// caps, then inserts the run and the engine job in tx.
func (s *Service) EnqueueTx(ctx context.Context, tx pgx.Tx, a Args, opts ...Opt) (RunRef, error) {
	k, ok := s.reg.Get(a.Kind())
	if !ok {
		return RunRef{}, apperr.New(apperr.InvalidParams, fmt.Sprintf("unknown job kind %q", a.Kind()))
	}
	spec := k.Spec
	o := EnqueueOpts{Trigger: TriggerSystem}
	for _, opt := range opts {
		opt(&o)
	}
	if err := checkTrigger(spec, o); err != nil {
		return RunRef{}, err
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return RunRef{}, fmt.Errorf("jobs: encode args: %w", err)
	}
	if err := k.ValidateArgs(raw); err != nil {
		return RunRef{}, apperr.Wrap(err, apperr.ValidationFailed, "job args do not match the kind's schema")
	}

	q := db.New(tx)
	now := s.clock.Now()

	if o.IdempotencyKey != "" {
		if ref, found, err := s.idempotent(ctx, q, spec.Kind, o, raw); err != nil || found {
			return ref, err
		}
	}
	if spec.Unique != nil {
		if ref, found, err := s.duplicate(ctx, q, spec, raw, now); err != nil || found {
			return ref, err
		}
	}
	if o.Trigger == TriggerUser && spec.MaxPending > 0 {
		if err := q.LockKey(ctx, lockKey("pending", spec.Kind)); err != nil {
			return RunRef{}, err
		}
		n, err := q.CountPending(ctx, spec.Kind)
		if err != nil {
			return RunRef{}, err
		}
		if n >= int64(spec.MaxPending) {
			return RunRef{}, apperr.New(apperr.QueueFull,
				fmt.Sprintf("%d %s jobs are already queued or running", spec.MaxPending, spec.Kind))
		}
	}

	var expires *time.Time
	if spec.ResultTTL > 0 {
		t := now.Add(spec.ResultTTL)
		expires = &t
	}
	var idem *string
	if o.IdempotencyKey != "" {
		idem = &o.IdempotencyKey
	}
	row, err := q.InsertRun(ctx, db.InsertRunParams{
		Kind:           spec.Kind,
		Args:           raw,
		ArgsVersion:    int32(spec.ArgsVersion), //nolint:gosec // small positive constant from code.
		Trigger:        string(o.Trigger),
		ScheduleID:     o.ScheduleID,
		UserID:         o.UserID,
		ParentID:       o.ParentID,
		RootID:         o.RootID,
		IdempotencyKey: idem,
		Queue:          spec.Queue,
		QueuedAt:       now,
		ExpiresAt:      expires,
	})
	if err != nil {
		return RunRef{}, fmt.Errorf("jobs: insert run: %w", err)
	}
	engineID, err := s.engine.InsertTx(ctx, tx, EngineJob{
		RunID:       row.ID,
		Kind:        spec.Kind,
		Queue:       spec.Queue,
		MaxAttempts: spec.MaxAttempts,
		Priority:    priority(o.Priority),
		RunAt:       o.RunAt,
	})
	if err != nil {
		return RunRef{}, fmt.Errorf("jobs: engine insert: %w", err)
	}
	if err := q.SetRunRootAndEngine(ctx, db.SetRunRootAndEngineParams{ID: row.ID, EngineJobID: &engineID}); err != nil {
		return RunRef{}, fmt.Errorf("jobs: set root: %w", err)
	}
	return RunRef{ID: row.ID, UUID: row.Uuid, Status: StatusQueued}, nil
}

func checkTrigger(spec Spec, o EnqueueOpts) error {
	if o.Trigger == TriggerUser {
		if !spec.UserTriggerable || !spec.Allows(TriggerUser) {
			return apperr.New(apperr.NotUserTriggerable, fmt.Sprintf("%s cannot be started by users", spec.Kind))
		}
		if o.UserID == nil {
			return errors.New("jobs: user trigger requires a user id")
		}
		return nil
	}
	// Chained and system runs follow from code, not from configuration.
	if o.Trigger != TriggerChain && o.Trigger != TriggerSystem && !spec.Allows(o.Trigger) {
		return fmt.Errorf("jobs: %s does not allow trigger %q", spec.Kind, o.Trigger)
	}
	if o.Trigger == TriggerSchedule && o.ScheduleID == nil {
		return errors.New("jobs: schedule trigger requires a schedule id")
	}
	if (o.Trigger == TriggerSchedule || o.Trigger == TriggerEvent) && o.UserID != nil {
		return fmt.Errorf("jobs: %s trigger cannot carry a user", o.Trigger)
	}
	return nil
}

// idempotent returns the earlier run for the same (user, kind, key). Reusing
// a key with different args is an error, not a silent duplicate.
func (s *Service) idempotent(ctx context.Context, q *db.Queries, kind string, o EnqueueOpts, raw []byte) (RunRef, bool, error) {
	user := "-"
	if o.UserID != nil {
		user = fmt.Sprint(*o.UserID)
	}
	if err := q.LockKey(ctx, lockKey("idem", user, kind, o.IdempotencyKey)); err != nil {
		return RunRef{}, false, err
	}
	prev, err := q.FindByIdempotencyKey(ctx, db.FindByIdempotencyKeyParams{
		Kind: kind, IdempotencyKey: &o.IdempotencyKey, UserID: o.UserID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return RunRef{}, false, nil
	}
	if err != nil {
		return RunRef{}, false, err
	}
	same, err := sameJSON(prev.Args, raw)
	if err != nil {
		return RunRef{}, false, err
	}
	if !same {
		return RunRef{}, false, apperr.New(apperr.IdempotencyMismatch,
			"Idempotency-Key was already used with different arguments")
	}
	return RunRef{ID: prev.ID, UUID: prev.Uuid, Status: Status(prev.Status), Duplicate: true}, true, nil
}

// duplicate finds an active run that matches the kind's UniqueOpts.
func (s *Service) duplicate(ctx context.Context, q *db.Queries, spec Spec, raw []byte, now time.Time) (RunRef, bool, error) {
	u := spec.Unique
	states := u.States
	if len(states) == 0 {
		states = []Status{StatusQueued, StatusRunning}
	}
	statusText := make([]string, len(states))
	for i, st := range states {
		statusText[i] = string(st)
	}
	since := time.Time{}
	if u.Period > 0 {
		since = now.Add(-u.Period)
	}

	// Empty ByArgs means the whole args object is the key.
	key := raw
	if len(u.ByArgs) > 0 {
		var err error
		if key, err = argsSubset(raw, u.ByArgs); err != nil {
			return RunRef{}, false, err
		}
	}
	if err := q.LockKey(ctx, lockKey("uniq", spec.Kind, string(key))); err != nil {
		return RunRef{}, false, err
	}

	var ref RunRef
	var err error
	if len(u.ByArgs) == 0 {
		var r db.FindDuplicateEqualRow
		r, err = q.FindDuplicateEqual(ctx, db.FindDuplicateEqualParams{Kind: spec.Kind, Args: key, Statuses: statusText, Since: since})
		ref = RunRef{ID: r.ID, UUID: r.Uuid, Status: Status(r.Status)}
	} else {
		var r db.FindDuplicateContainingRow
		r, err = q.FindDuplicateContaining(ctx, db.FindDuplicateContainingParams{Kind: spec.Kind, Subset: key, Statuses: statusText, Since: since})
		ref = RunRef{ID: r.ID, UUID: r.Uuid, Status: Status(r.Status)}
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return RunRef{}, false, nil
	case err != nil:
		return RunRef{}, false, err
	}
	ref.Duplicate = true
	return ref, true, nil
}

// argsSubset keeps only the named top-level fields of raw. Absent fields are
// encoded as null, so a missing value never matches every run.
func argsSubset(raw []byte, fields []string) ([]byte, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, fmt.Errorf("jobs: unique args: %w", err)
	}
	out := make(map[string]json.RawMessage, len(fields))
	for _, f := range fields {
		if v, ok := all[f]; ok {
			out[f] = v
		} else {
			out[f] = json.RawMessage("null")
		}
	}
	return json.Marshal(out)
}

func sameJSON(a, b []byte) (bool, error) {
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, &y); err != nil {
		return false, err
	}
	return reflect.DeepEqual(x, y), nil
}

// lockKey hashes parts into a transaction-scoped advisory lock key.
func lockKey(parts ...string) int64 {
	h := fnv.New64a()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return int64(h.Sum64()) //nolint:gosec // wraparound is fine for a hash.
}

func priority(p int) int {
	if p < 1 || p > 4 {
		return 2
	}
	return p
}
