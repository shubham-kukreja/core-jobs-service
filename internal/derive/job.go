package derive

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
)

// KindCascade is Job 2.
const KindCascade = "derive.cascade"

// CascadeArgs are derive.cascade's arguments. Add fields, never rename.
type CascadeArgs struct {
	Mode              CascadeMode `json:"mode"`
	BatchID           *uuid.UUID  `json:"batch_id,omitempty"`
	Series            []string    `json:"series,omitempty"`
	IncludeDownstream bool        `json:"include_downstream,omitempty"`
}

func (CascadeArgs) Kind() string { return KindCascade }

// CascadeSpec is derive.cascade's static definition. Failures are retryable:
// the input hash makes re-runs idempotent.
var CascadeSpec = jobs.Spec{
	Kind:            KindCascade,
	Queue:           jobs.QueueCompute,
	Triggers:        []jobs.TriggerType{jobs.TriggerEvent, jobs.TriggerUser, jobs.TriggerSystem, jobs.TriggerChain},
	UserTriggerable: true,
	Roles:           []string{"admin"},
	MaxAttempts:     5,
	Timeout:         30 * time.Minute,
	Concurrency:     1, // layers read each other's output; one cascade at a time
	ArgsVersion:     1,
}

// RegisterCascade registers derive.cascade with c as its engine.
func RegisterCascade(r *jobs.Registry, c Cascade) {
	jobs.Register(r, CascadeSpec, jobs.HandlerFunc[CascadeArgs](func(ctx context.Context, rc *jobs.RunCtx, a CascadeArgs) (jobs.Outcome, error) {
		req := CascadeRequest{Mode: a.Mode, SeriesKeys: a.Series, IncludeDownstream: a.IncludeDownstream}
		if a.Mode == ModeIncremental {
			if a.BatchID == nil {
				return jobs.Outcome{}, fmt.Errorf("%w: incremental needs batch_id", ErrInvalidParams)
			}
			req.BatchID = *a.BatchID
		}
		res, err := c.Run(ctx, req, rc.RunID, rc.Progress)
		if err != nil {
			return jobs.Outcome{}, err
		}
		return jobs.Outcome{Summary: map[string]any{
			"series": res.Series, "rows_written": res.RowsWritten, "rows_skipped": res.RowsSkipped,
		}}, nil
	}))
}

// Subscriber starts an incremental cascade for every primary write. Writes
// the cascade made itself (origin = cascade) are not re-cascaded.
type Subscriber struct{}

func (Subscriber) Topic() string { return datapoints.TopicWritten }

func (Subscriber) Handle(_ context.Context, ev jobs.Event) ([]jobs.Args, error) {
	var w datapoints.WrittenEvent
	if err := json.Unmarshal(ev.Payload, &w); err != nil {
		return nil, fmt.Errorf("derive: decode %s: %w", ev.Topic, err)
	}
	if w.Origin == datapoints.OriginCascade || len(w.SeriesIDs) == 0 {
		return nil, nil
	}
	id := w.BatchID
	return []jobs.Args{CascadeArgs{Mode: ModeIncremental, BatchID: &id}}, nil
}
