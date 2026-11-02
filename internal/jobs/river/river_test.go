package river

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
)

func TestToRiverError(t *testing.T) {
	boom := errors.New("boom")
	if toRiverError(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	var cancel *river.JobCancelError
	if !errors.As(toRiverError(jobs.Permanent(boom)), &cancel) {
		t.Fatal("permanent must cancel the River job")
	}
	var snooze *river.JobSnoozeError
	if !errors.As(toRiverError(jobs.Snooze(time.Minute)), &snooze) || snooze.Duration != time.Minute {
		t.Fatal("snooze must map to JobSnooze")
	}
	if got := toRiverError(boom); !errors.Is(got, boom) || errors.As(got, &cancel) {
		t.Fatal("other errors retry")
	}
}

type qArgs struct{}

func (qArgs) Kind() string { return "test.backtest" }

func TestQueues(t *testing.T) {
	reg := jobs.NewRegistry()
	jobs.Register(reg, jobs.Spec{
		Kind: "test.backtest", Queue: jobs.QueueBacktest, Triggers: []jobs.TriggerType{jobs.TriggerSystem},
		MaxAttempts: 1, Timeout: time.Minute, ArgsVersion: 1, Concurrency: 1,
	}, jobs.HandlerFunc[qArgs](func(context.Context, *jobs.RunCtx, qArgs) (jobs.Outcome, error) {
		return jobs.Outcome{}, nil
	}))
	q := queues(reg, nil)
	if q[jobs.QueueBacktest].MaxWorkers != 1 || q[jobs.QueueSystem].MaxWorkers != 2 {
		t.Fatalf("queues = %+v", q)
	}
	if q = queues(reg, map[string]int{jobs.QueueBacktest: 3}); q[jobs.QueueBacktest].MaxWorkers != 3 {
		t.Fatalf("override ignored: %+v", q)
	}
}
