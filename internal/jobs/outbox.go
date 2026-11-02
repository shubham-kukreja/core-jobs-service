package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
)

// OutboxSource is the outbox the dispatcher drains. jobs does not import the
// datapoints module; wiring adapts datapoints.Outbox to this interface.
type OutboxSource interface {
	// Claim locks up to limit undispatched events in tx (FOR UPDATE SKIP LOCKED).
	Claim(ctx context.Context, tx pgx.Tx, limit int) ([]Event, error)
	MarkDispatched(ctx context.Context, tx pgx.Tx, ids []int64) error
}

// Subscribers maps outbox topics to subscribers.
type Subscribers struct {
	mu      sync.RWMutex
	byTopic map[string][]Subscriber
}

// NewSubscribers returns a registry holding subs.
func NewSubscribers(subs ...Subscriber) *Subscribers {
	r := &Subscribers{byTopic: map[string][]Subscriber{}}
	for _, s := range subs {
		r.Register(s)
	}
	return r
}

// Register adds a subscriber for its topic.
func (r *Subscribers) Register(s Subscriber) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byTopic[s.Topic()] = append(r.byTopic[s.Topic()], s)
}

// For returns the subscribers of topic.
func (r *Subscribers) For(topic string) []Subscriber {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byTopic[topic]
}

// Topics lists topics with subscribers.
func (r *Subscribers) Topics() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byTopic))
	for t := range r.byTopic {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// DispatchBatch is how many events one dispatch claims.
const DispatchBatch = 100

// Dispatcher turns outbox events into jobs (trigger = event). Each event is
// claimed, fanned out to its subscribers' jobs and marked dispatched in one
// transaction, so every event is enqueued exactly once.
type Dispatcher struct {
	pool Pool
	src  OutboxSource
	subs *Subscribers
	enq  Enqueuer
	log  *slog.Logger
}

// NewDispatcher builds the dispatcher.
func NewDispatcher(pool Pool, src OutboxSource, subs *Subscribers, enq Enqueuer, log *slog.Logger) *Dispatcher {
	return &Dispatcher{pool: pool, src: src, subs: subs, enq: enq, log: log}
}

// Dispatch drains one batch and returns how many events were dispatched.
// An event whose subscriber fails stays undispatched and is retried on the
// next call; it does not block the other events in the batch.
func (d *Dispatcher) Dispatch(ctx context.Context) (int, error) {
	dispatched := 0
	err := database.InTx(ctx, d.pool, func(tx pgx.Tx) error {
		events, err := d.src.Claim(ctx, tx, DispatchBatch)
		if err != nil {
			return fmt.Errorf("jobs: claim outbox: %w", err)
		}
		var done []int64
		for _, ev := range events {
			sp, err := tx.Begin(ctx)
			if err != nil {
				return err
			}
			if err := d.fanOut(ctx, sp, ev); err != nil {
				_ = sp.Rollback(ctx)
				d.log.Error("outbox event not dispatched; will retry", "event", ev.ID, "topic", ev.Topic, "err", err)
				continue
			}
			if err := sp.Commit(ctx); err != nil {
				return err
			}
			done = append(done, ev.ID)
		}
		if len(done) == 0 {
			return nil
		}
		if err := d.src.MarkDispatched(ctx, tx, done); err != nil {
			return fmt.Errorf("jobs: mark dispatched: %w", err)
		}
		dispatched = len(done)
		return nil
	})
	return dispatched, err
}

func (d *Dispatcher) fanOut(ctx context.Context, tx pgx.Tx, ev Event) error {
	for _, s := range d.subs.For(ev.Topic) {
		args, err := s.Handle(ctx, ev)
		if err != nil {
			return fmt.Errorf("subscriber %T: %w", s, err)
		}
		for _, a := range args {
			if _, err := d.enq.EnqueueTx(ctx, tx, a, WithTrigger(TriggerEvent)); err != nil {
				return fmt.Errorf("enqueue %s: %w", a.Kind(), err)
			}
		}
	}
	return nil
}
