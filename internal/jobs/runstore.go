package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/jobs/internal/db"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
)

// storedRun is the part of a job_runs row the executor needs.
type storedRun struct {
	ID      int64
	UUID    uuid.UUID
	Kind    string
	Args    json.RawMessage
	Trigger TriggerType
	UserID  *int64
	RootID  *int64
	Status  Status
}

// runStore is the executor's persistence port, faked in unit tests.
type runStore interface {
	get(ctx context.Context, id int64) (storedRun, error)
	markRunning(ctx context.Context, id int64, attempt int, now time.Time) (bool, error)
	cancelRequested(ctx context.Context, id int64) (bool, error)
	progress(ctx context.Context, id int64, pct int, msg string) error
	// complete persists a success and enqueues out.Next as chained runs, in one tx.
	complete(ctx context.Context, run storedRun, out Outcome, now time.Time) error
	fail(ctx context.Context, id int64, status Status, re RunError, now time.Time) error
	requeue(ctx context.Context, id int64, msg string, re *RunError) error
}

type pgRunStore struct {
	pool Pool
	enq  Enqueuer
}

func (s *pgRunStore) q() *db.Queries { return db.New(s.pool) }

func (s *pgRunStore) get(ctx context.Context, id int64) (storedRun, error) {
	r, err := s.q().GetRunByID(ctx, id)
	if err != nil {
		return storedRun{}, err
	}
	return storedRun{
		ID: r.ID, UUID: r.Uuid, Kind: r.Kind, Args: r.Args, Trigger: TriggerType(r.Trigger),
		UserID: r.UserID, RootID: r.RootID, Status: Status(r.Status),
	}, nil
}

func (s *pgRunStore) markRunning(ctx context.Context, id int64, attempt int, now time.Time) (bool, error) {
	_, err := s.q().MarkRunning(ctx, db.MarkRunningParams{ID: id, Attempt: int32(attempt), Now: &now}) //nolint:gosec // attempts are small.
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *pgRunStore) cancelRequested(ctx context.Context, id int64) (bool, error) {
	return s.q().IsCancelRequested(ctx, id)
}

func (s *pgRunStore) progress(ctx context.Context, id int64, pct int, msg string) error {
	var m *string
	if msg != "" {
		m = &msg
	}
	return s.q().UpdateProgress(ctx, db.UpdateProgressParams{ID: id, Progress: int16(pct), Message: m}) //nolint:gosec // clamped to 0..100.
}

func (s *pgRunStore) complete(ctx context.Context, run storedRun, out Outcome, now time.Time) error {
	var result []byte
	if out.Summary != nil {
		b, err := json.Marshal(out.Summary)
		if err != nil {
			return fmt.Errorf("jobs: encode outcome: %w", err)
		}
		result = b
	}
	var uri *string
	if out.BlobURI != "" {
		uri = &out.BlobURI
	}
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := db.New(tx).FinishRun(ctx, db.FinishRunParams{
			ID: run.ID, Status: string(StatusSucceeded), Result: result, ResultUri: uri, Now: &now,
		}); err != nil {
			return err
		}
		for _, next := range out.Next {
			if _, err := s.enq.EnqueueTx(ctx, tx, next, WithTrigger(TriggerChain), WithParent(run.ID, rootOf(run))); err != nil {
				return fmt.Errorf("jobs: chain %s: %w", next.Kind(), err)
			}
		}
		return nil
	})
}

func (s *pgRunStore) fail(ctx context.Context, id int64, status Status, re RunError, now time.Time) error {
	b, err := json.Marshal(re)
	if err != nil {
		return err
	}
	msg := re.Message
	return s.q().FinishRun(ctx, db.FinishRunParams{ID: id, Status: string(status), Error: b, Message: &msg, Now: &now})
}

func (s *pgRunStore) requeue(ctx context.Context, id int64, msg string, re *RunError) error {
	var b []byte
	if re != nil {
		var err error
		if b, err = json.Marshal(re); err != nil {
			return err
		}
	}
	return s.q().RequeueRun(ctx, db.RequeueRunParams{ID: id, Message: &msg, Error: b})
}
