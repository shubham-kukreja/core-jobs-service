package datapoints

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/civil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
)

// Retraction rows take the target's series, date and source; values are null.
const (
	// The runtime role cannot SELECT … FOR UPDATE on append-only tables, so
	// retractions of one datapoint are serialised with an advisory lock.
	sqlLockRetract = `SELECT pg_advisory_xact_lock(hashtextextended('dp.retract:' || $1::text, 0))`

	sqlRetractTarget = `
SELECT d.series_id, d.obs_date, d.source_id, d.status,
       EXISTS (SELECT 1 FROM dp.datapoints r WHERE r.supersedes_id = d.id AND r.status = 'retracted')
FROM dp.datapoints d WHERE d.id = $1`

	sqlInsertRetraction = `
INSERT INTO dp.datapoints (series_id, obs_date, known_at, source_id, status, supersedes_id,
                           batch_id, job_run_id, actor_id, note)
VALUES ($1, $2, $3, $4, 'retracted', $5, $6, $7, $8, $9)`
)

func (s *Store) Retract(ctx context.Context, tx pgx.Tx, meta WriteMeta, ids []int64, reason string) error {
	reason = strings.TrimSpace(reason)
	switch {
	case len(ids) == 0:
		return apperr.New(apperr.ValidationFailed, "no datapoints to retract")
	case len(ids) > MaxBatch:
		return apperr.New(apperr.ValidationFailed, fmt.Sprintf("at most %d datapoints per retraction", MaxBatch))
	case reason == "" || len(reason) > maxNoteLen:
		return apperr.New(apperr.ValidationFailed, "a reason of 1–2000 characters is required",
			apperr.Detail{Field: "reason", Reason: "invalid_length"})
	case meta.ActorID == nil && meta.JobRunID == nil:
		return apperr.New(apperr.InvalidParams, "retractions require an actor or a job run")
	}
	if meta.Origin == "" {
		meta.Origin = OriginPrimary
	}

	batch := uuid.New()
	now := s.clock.Now().UTC()
	ev := WrittenEvent{BatchID: batch, Origin: meta.Origin}
	seriesSet, layerSet := map[int64]bool{}, map[string]bool{}
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, err := tx.Exec(ctx, sqlLockRetract, id); err != nil {
			return fmt.Errorf("datapoints: lock retraction: %w", err)
		}
		var (
			seriesID, sourceID int64
			obs                time.Time
			status             string
			retracted          bool
		)
		err := tx.QueryRow(ctx, sqlRetractTarget, id).Scan(&seriesID, &obs, &sourceID, &status, &retracted)
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.Wrap(ErrNotFound, apperr.NotFound, fmt.Sprintf("datapoint %d not found", id))
		}
		if err != nil {
			return fmt.Errorf("datapoints: retraction target: %w", err)
		}
		if status != "active" || retracted {
			return apperr.Wrap(ErrAlreadyRetracted, apperr.Conflict, fmt.Sprintf("datapoint %d is already retracted", id))
		}
		if _, err := tx.Exec(ctx, sqlInsertRetraction, seriesID, obs, now, sourceID, id, batch,
			meta.JobRunID, meta.ActorID, reason); err != nil {
			return fmt.Errorf("datapoints: insert retraction: %w", err)
		}
		if !seriesSet[seriesID] {
			seriesSet[seriesID] = true
			ser, err := s.cat.SeriesByIDs(ctx, []int64{seriesID})
			if err != nil {
				return err
			}
			layerSet[string(ser[seriesID].Layer)] = true
		}
		d := civil.DateOf(obs)
		if ev.MinDate.IsZero() || d.Before(ev.MinDate) {
			ev.MinDate = d
		}
		if ev.MaxDate.IsZero() || d.After(ev.MaxDate) {
			ev.MaxDate = d
		}
	}
	for id := range seriesSet {
		ev.SeriesIDs = append(ev.SeriesIDs, id)
	}
	for l := range layerSet {
		ev.Layers = append(ev.Layers, l)
	}
	sortEvent(&ev)
	// Retractions change resolved values, so they notify the cascade too.
	return insertEvent(ctx, tx, ev)
}
