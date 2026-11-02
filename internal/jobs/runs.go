package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/jobs/internal/db"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
)

// MaxTreeDepth caps GET /v1/jobs/{id}/tree.
const MaxTreeDepth = 10

// ErrNotFound is returned for unknown runs and schedules.
var ErrNotFound = apperr.New(apperr.NotFound, "job not found")

// RunStore implements Runs.
type RunStore struct {
	pool      Pool
	enq       Enqueuer
	clock     clock.Clock
	watchPoll time.Duration
}

var _ Runs = (*RunStore)(nil)

// NewRunStore builds the run repository. enq is used by Retry.
func NewRunStore(pool Pool, enq Enqueuer, clk clock.Clock) *RunStore {
	return &RunStore{pool: pool, enq: enq, clock: clk, watchPoll: time.Second}
}

// Get returns one run.
func (s *RunStore) Get(ctx context.Context, id uuid.UUID) (JobRun, error) {
	r, err := db.New(s.pool).GetRunByUUID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return JobRun{}, ErrNotFound
	}
	if err != nil {
		return JobRun{}, err
	}
	return toJobRun(r)
}

// List returns runs newest first. The keyset position is the last run's id.
func (s *RunStore) List(ctx context.Context, f RunFilter) (paging.Page[JobRun], error) {
	p := f.Page.Normalize()
	params := db.ListRunsParams{RootID: f.RootID, Since: f.Since, Lim: int32(p.Limit + 1)} //nolint:gosec // clamped by Normalize.
	if f.Kind != "" {
		params.Kind = &f.Kind
	}
	if f.Status != "" {
		st := string(f.Status)
		params.Status = &st
	}
	if f.Trigger != "" {
		tr := string(f.Trigger)
		params.Trigger = &tr
	}
	if p.After != "" {
		id, err := strconv.ParseInt(p.After, 10, 64)
		if err != nil {
			return paging.Page[JobRun]{}, apperr.New(apperr.InvalidParams, "invalid cursor")
		}
		params.BeforeID = &id
	}
	rows, err := db.New(s.pool).ListRuns(ctx, params)
	if err != nil {
		return paging.Page[JobRun]{}, err
	}
	var page paging.Page[JobRun]
	if len(rows) > p.Limit {
		rows = rows[:p.Limit]
		page.Next = strconv.FormatInt(rows[len(rows)-1].ID, 10)
	}
	for _, r := range rows {
		jr, err := toJobRun(r)
		if err != nil {
			return paging.Page[JobRun]{}, err
		}
		page.Items = append(page.Items, jr)
	}
	return page, nil
}

// Tree returns a run and every run it caused, up to maxDepth levels.
func (s *RunStore) Tree(ctx context.Context, id uuid.UUID, maxDepth int) (RunTree, error) {
	if maxDepth <= 0 || maxDepth > MaxTreeDepth {
		maxDepth = MaxTreeDepth
	}
	q := db.New(s.pool)
	root, err := q.GetRunByUUID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunTree{}, ErrNotFound
	}
	if err != nil {
		return RunTree{}, err
	}
	rows, err := q.RunTree(ctx, db.RunTreeParams{RootID: root.ID, MaxDepth: int32(maxDepth)})
	if err != nil {
		return RunTree{}, err
	}
	runs := make([]JobRun, 0, len(rows))
	for _, r := range rows {
		jr, err := toJobRun(db.JobsJobRun{
			ID: r.ID, Uuid: r.Uuid, Kind: r.Kind, Args: r.Args, ArgsVersion: r.ArgsVersion, Trigger: r.Trigger,
			ScheduleID: r.ScheduleID, UserID: r.UserID, ParentID: r.ParentID, RootID: r.RootID,
			Queue: r.Queue, Status: r.Status, Attempt: r.Attempt, Progress: r.Progress, Message: r.Message,
			Result: r.Result, ResultUri: r.ResultUri, Error: r.Error, CancelRequested: r.CancelRequested,
			QueuedAt: r.QueuedAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
		})
		if err != nil {
			return RunTree{}, err
		}
		runs = append(runs, jr)
	}
	return buildTree(root.ID, runs), nil
}

// buildTree nests runs under their parents, keeping id order among siblings.
func buildTree(rootID int64, runs []JobRun) RunTree {
	children := map[int64][]JobRun{}
	var root JobRun
	for _, r := range runs {
		if r.ID == rootID {
			root = r
			continue
		}
		if r.ParentID != nil {
			children[*r.ParentID] = append(children[*r.ParentID], r)
		}
	}
	var build func(r JobRun) RunTree
	build = func(r JobRun) RunTree {
		t := RunTree{JobRun: r}
		for _, c := range children[r.ID] {
			t.Children = append(t.Children, build(c))
		}
		return t
	}
	return build(root)
}

// RequestCancel cancels a queued run immediately and flags a running one;
// the executor's watcher stops it within one poll interval.
func (s *RunStore) RequestCancel(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	q := db.New(tx)
	r, err := q.GetRunByUUID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	now := s.clock.Now()
	_, err = q.RequestCancel(ctx, db.RequestCancelParams{ID: r.ID, Now: &now})
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.New(apperr.Conflict, fmt.Sprintf("job is already %s", r.Status))
	}
	return err
}

// Retry starts a new run with the same args as a failed or cancelled one.
// The new run is a child of the original, so it appears in its tree.
// actorID is recorded by the caller's audit entry.
func (s *RunStore) Retry(ctx context.Context, tx pgx.Tx, id uuid.UUID, _ int64) (RunRef, error) {
	r, err := db.New(tx).GetRunByUUID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunRef{}, ErrNotFound
	}
	if err != nil {
		return RunRef{}, err
	}
	if st := Status(r.Status); st != StatusFailed && st != StatusCancelled {
		return RunRef{}, apperr.New(apperr.Conflict, "only failed or cancelled jobs can be retried")
	}
	root := r.ID
	if r.RootID != nil {
		root = *r.RootID
	}
	return s.enq.EnqueueTx(ctx, tx, RawArgs{KindName: r.Kind, JSON: r.Args},
		WithTrigger(TriggerSystem), WithParent(r.ID, root))
}

// Watch polls the run and emits progress and status changes until it
// finishes or ctx ends. The channel closes when watching stops.
func (s *RunStore) Watch(ctx context.Context, id uuid.UUID) (<-chan RunUpdate, error) {
	first, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	ch := make(chan RunUpdate, 8)
	go func() {
		defer close(ch)
		emit := func(u RunUpdate) bool {
			select {
			case ch <- u:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if !emit(statusUpdate(first)) || terminal(first.Status) {
			return
		}
		t := time.NewTicker(s.watchPoll)
		defer t.Stop()
		prev := first
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			cur, err := s.Get(ctx, id)
			if err != nil {
				return
			}
			for _, u := range diffUpdates(prev, cur) {
				if !emit(u) {
					return
				}
			}
			if terminal(cur.Status) {
				return
			}
			prev = cur
		}
	}()
	return ch, nil
}

// diffUpdates returns the SSE events between two observations of a run.
func diffUpdates(prev, cur JobRun) []RunUpdate {
	var out []RunUpdate
	if cur.Progress != prev.Progress || cur.Message != prev.Message {
		out = append(out, RunUpdate{Kind: "progress", Progress: cur.Progress, Message: cur.Message})
	}
	if cur.Status != prev.Status {
		out = append(out, statusUpdate(cur))
	}
	return out
}

func statusUpdate(r JobRun) RunUpdate {
	return RunUpdate{Kind: "status", Status: r.Status, Progress: r.Progress, Message: r.Message, Result: r.Result}
}

func terminal(s Status) bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusSkipped:
		return true
	}
	return false
}

func toJobRun(r db.JobsJobRun) (JobRun, error) {
	jr := JobRun{
		ID: r.ID, UUID: r.Uuid, Kind: r.Kind, Args: r.Args, ArgsVersion: int(r.ArgsVersion),
		Trigger: TriggerType(r.Trigger), ScheduleID: r.ScheduleID, UserID: r.UserID,
		ParentID: r.ParentID, RootID: r.RootID, Queue: r.Queue, Status: Status(r.Status),
		Attempt: int(r.Attempt), Progress: int(r.Progress), Result: r.Result,
		CancelRequested: r.CancelRequested, QueuedAt: r.QueuedAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
	}
	if r.Message != nil {
		jr.Message = *r.Message
	}
	if r.ResultUri != nil {
		jr.ResultURI = *r.ResultUri
	}
	if len(r.Error) > 0 {
		var re RunError
		if err := json.Unmarshal(r.Error, &re); err != nil {
			return JobRun{}, fmt.Errorf("jobs: decode run error: %w", err)
		}
		jr.Error = &re
	}
	return jr, nil
}
