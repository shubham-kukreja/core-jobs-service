package jobs

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

type fakeEnqueuer struct {
	got  []Args
	opts []EnqueueOpts
}

func (f *fakeEnqueuer) Enqueue(ctx context.Context, a Args, o ...Opt) (RunRef, error) {
	return f.EnqueueTx(ctx, nil, a, o...)
}

func (f *fakeEnqueuer) EnqueueTx(_ context.Context, _ pgx.Tx, a Args, o ...Opt) (RunRef, error) {
	var eo EnqueueOpts
	for _, opt := range o {
		opt(&eo)
	}
	f.got = append(f.got, a)
	f.opts = append(f.opts, eo)
	return RunRef{ID: int64(len(f.got))}, nil
}

type fnSubscriber struct {
	topic string
	fn    func(Event) ([]Args, error)
}

func (s fnSubscriber) Topic() string { return s.topic }
func (s fnSubscriber) Handle(_ context.Context, ev Event) ([]Args, error) {
	return s.fn(ev)
}

func TestFanOutEnqueuesEventJobs(t *testing.T) {
	enq := &fakeEnqueuer{}
	subs := NewSubscribers(
		fnSubscriber{"datapoints.written", func(Event) ([]Args, error) { return []Args{execArgs{}}, nil }},
		fnSubscriber{"datapoints.written", func(Event) ([]Args, error) { return nil, nil }},
		fnSubscriber{"run.completed", func(Event) ([]Args, error) { return []Args{execArgs{}}, nil }},
	)
	d := NewDispatcher(nil, nil, subs, enq, nil)
	if err := d.fanOut(context.Background(), nil, Event{ID: 1, Topic: "datapoints.written"}); err != nil {
		t.Fatal(err)
	}
	if len(enq.got) != 1 || enq.opts[0].Trigger != TriggerEvent {
		t.Fatalf("enqueued %v with %+v", enq.got, enq.opts)
	}
	if got := subs.Topics(); len(got) != 2 || got[0] != "datapoints.written" {
		t.Fatalf("topics = %v", got)
	}
}

func TestFanOutStopsOnSubscriberError(t *testing.T) {
	enq := &fakeEnqueuer{}
	boom := errors.New("bad payload")
	subs := NewSubscribers(fnSubscriber{"t", func(Event) ([]Args, error) { return nil, boom }})
	d := NewDispatcher(nil, nil, subs, enq, nil)
	if err := d.fanOut(context.Background(), nil, Event{Topic: "t"}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if len(enq.got) != 0 {
		t.Fatal("nothing should be enqueued")
	}
}
