package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
)

// Metrics observes job executions. Implemented with Prometheus in Phase 4.
type Metrics interface {
	JobStarted(kind string)
	JobFinished(kind string, status Status, d time.Duration)
}

// NopMetrics discards observations.
type NopMetrics struct{}

func (NopMetrics) JobStarted(string)                         {}
func (NopMetrics) JobFinished(string, Status, time.Duration) {}

// ExecutorConfig tunes the middleware chain.
type ExecutorConfig struct {
	CancelPoll       time.Duration // how often cancel_requested is polled; default 2s
	ProgressInterval time.Duration // minimum gap between progress writes; default 1s
	Metrics          Metrics
	Log              *slog.Logger
	Clock            clock.Clock
}

func (c ExecutorConfig) withDefaults() ExecutorConfig {
	if c.CancelPoll <= 0 {
		c.CancelPoll = 2 * time.Second
	}
	if c.ProgressInterval <= 0 {
		c.ProgressInterval = time.Second
	}
	if c.Metrics == nil {
		c.Metrics = NopMetrics{}
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
	if c.Clock == nil {
		c.Clock = clock.System{}
	}
	return c
}

// Execution identifies one attempt handed over by the engine.
type Execution struct {
	RunID       int64
	Attempt     int
	MaxAttempts int
}

// Executor runs one job attempt through the middleware chain:
// recover → job_runs bookkeeping → metrics → timeout → cancellation watcher
// → handler → persist outcome and chain Next in one transaction.
//
// Execute returns nil when the engine should consider the job done, a
// Snooze error to re-run later, a Permanent error to stop retrying, or any
// other error to retry.
type Executor struct {
	reg   *Registry
	store runStore
	cfg   ExecutorConfig
}

// NewExecutor builds the executor. enq enqueues chained follow-ups.
func NewExecutor(pool Pool, reg *Registry, enq Enqueuer, cfg ExecutorConfig) *Executor {
	return newExecutor(reg, &pgRunStore{pool: pool, enq: enq}, cfg)
}

func newExecutor(reg *Registry, store runStore, cfg ExecutorConfig) *Executor {
	return &Executor{reg: reg, store: store, cfg: cfg.withDefaults()}
}

var (
	errCancelRequested = errors.New("jobs: cancel requested")
	errTimeout         = errors.New("jobs: timed out")
	errPanic           = errors.New("jobs: handler panicked")
)

// Execute runs one attempt.
func (e *Executor) Execute(ctx context.Context, ex Execution) error {
	run, err := e.store.get(ctx, ex.RunID)
	if err != nil {
		return fmt.Errorf("jobs: load run %d: %w", ex.RunID, err)
	}
	log := e.cfg.Log.With("job_run", run.UUID, "kind", run.Kind, "attempt", ex.Attempt)

	k, ok := e.reg.Get(run.Kind)
	if !ok {
		log.Error("job kind not registered")
		_ = e.store.fail(ctx, run.ID, StatusFailed, RunError{Code: "unknown_kind", Message: "job kind is not available"}, e.cfg.Clock.Now())
		return Permanent(fmt.Errorf("jobs: unknown kind %q", run.Kind))
	}

	started := e.cfg.Clock.Now()
	runnable, err := e.store.markRunning(ctx, run.ID, ex.Attempt, started)
	if err != nil {
		return fmt.Errorf("jobs: mark running: %w", err)
	}
	if !runnable {
		// Cancelled while queued, or already finished: nothing to do.
		log.Info("job skipped", "status", run.Status)
		return nil
	}
	e.cfg.Metrics.JobStarted(run.Kind)

	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	runCtx, cancelTimeout := context.WithTimeoutCause(runCtx, k.Spec.Timeout, errTimeout)
	defer cancelTimeout()
	stopWatch := e.watchCancel(runCtx, run.ID, cancel, log)
	defer stopWatch()

	progress := e.newProgress(ctx, run.ID, log)
	rc := &RunCtx{
		RunID:     run.ID,
		RunUUID:   run.UUID,
		RootID:    rootOf(run),
		Attempt:   ex.Attempt,
		Trigger:   run.Trigger,
		UserID:    run.UserID,
		Log:       log,
		Progress:  progress,
		Cancelled: func() bool { return runCtx.Err() != nil },
	}

	out, herr := invokeSafely(runCtx, k, rc, run.Args, log)
	cause := context.Cause(runCtx)
	finished := e.cfg.Clock.Now()

	status, ret := e.settle(ctx, run, ex, out, herr, cause, finished, log)
	e.cfg.Metrics.JobFinished(run.Kind, status, finished.Sub(started))
	return ret
}

// settle records the attempt's outcome and decides what the engine does next.
func (e *Executor) settle(ctx context.Context, run storedRun, ex Execution, out Outcome, herr, cause error, now time.Time, log *slog.Logger) (Status, error) {
	// Persist with a context that survives the run's cancellation.
	pctx := context.WithoutCancel(ctx)

	switch {
	case herr == nil:
		// The handler finished its work; a cancel or timeout that arrived at
		// the same moment does not undo it.
		if err := e.store.complete(pctx, run, out, now); err != nil {
			// Not persisted: retry the attempt rather than lose the outcome.
			log.Error("persist outcome failed", "err", err)
			return StatusFailed, Retryable(err)
		}
		log.Info("job succeeded")
		return StatusSucceeded, nil

	case errors.Is(cause, errCancelRequested):
		log.Info("job cancelled")
		if err := e.store.fail(pctx, run.ID, StatusCancelled, RunError{Code: "cancelled", Message: "cancelled on request"}, now); err != nil {
			return StatusCancelled, err
		}
		return StatusCancelled, nil

	case cause != nil && !errors.Is(cause, errTimeout) && ctx.Err() != nil:
		// The worker is shutting down: put the run back without spending an attempt.
		log.Warn("job interrupted by shutdown")
		_ = e.store.requeue(pctx, run.ID, "interrupted by shutdown", nil)
		return StatusQueued, Snooze(0)
	}

	err := herr
	if errors.Is(cause, errTimeout) {
		err = errTimeout
	}

	if d, ok := SnoozeDuration(err); ok {
		_ = e.store.requeue(pctx, run.ID, "snoozed for "+d.String(), nil)
		return StatusQueued, err
	}

	re := safeError(err)
	final := IsPermanent(err) || ex.Attempt >= ex.MaxAttempts
	re.Retryable = !final
	if final {
		log.Error("job failed", "err", err)
		if perr := e.store.fail(pctx, run.ID, StatusFailed, re, now); perr != nil {
			return StatusFailed, perr
		}
		return StatusFailed, Permanent(err)
	}
	log.Warn("job attempt failed; will retry", "err", err)
	_ = e.store.requeue(pctx, run.ID, "retrying after error", &re)
	return StatusQueued, Retryable(err)
}

// invokeSafely runs the handler, converting a panic into a permanent error.
// The stack goes to the log only, never into the stored error.
func invokeSafely(ctx context.Context, k Kind, rc *RunCtx, args json.RawMessage, log *slog.Logger) (out Outcome, err error) {
	defer func() {
		if p := recover(); p != nil {
			log.Error("job handler panicked", "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
			out, err = Outcome{}, Permanent(errPanic)
		}
	}()
	return k.Invoke(ctx, rc, args)
}

// safeError maps err to a message that is safe to store and show users.
func safeError(err error) RunError {
	switch {
	case errors.Is(err, errTimeout):
		return RunError{Code: "timeout", Message: "job timed out"}
	case errors.Is(err, errPanic):
		return RunError{Code: string(apperr.Internal), Message: "internal error"}
	}
	if ae, ok := apperr.As(err); ok {
		return RunError{Code: string(ae.Code), Message: ae.Message}
	}
	return RunError{Code: string(apperr.Internal), Message: "internal error"}
}

// watchCancel polls cancel_requested and cancels the run when it is set.
func (e *Executor) watchCancel(ctx context.Context, runID int64, cancel context.CancelCauseFunc, log *slog.Logger) (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(e.cfg.CancelPoll)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				req, err := e.store.cancelRequested(ctx, runID)
				if err != nil {
					if ctx.Err() == nil {
						log.Warn("cancel poll failed", "err", err)
					}
					continue
				}
				if req {
					cancel(errCancelRequested)
					return
				}
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// newProgress returns a throttled progress reporter: at most one write per
// ProgressInterval, except 100% which is always written. Writes use the
// attempt's context (not the run's), so they still land while cancelling.
func (e *Executor) newProgress(ctx context.Context, runID int64, log *slog.Logger) func(int, string) {
	var (
		mu   sync.Mutex
		last time.Time
	)
	return func(pct int, msg string) {
		pct = min(max(pct, 0), 100)
		if len(msg) > 500 {
			msg = msg[:500]
		}
		mu.Lock()
		now := e.cfg.Clock.Now()
		if pct < 100 && !last.IsZero() && now.Sub(last) < e.cfg.ProgressInterval {
			mu.Unlock()
			return
		}
		last = now
		mu.Unlock()
		if err := e.store.progress(ctx, runID, pct, msg); err != nil {
			log.Warn("progress write failed", "err", err)
		}
	}
}

func rootOf(r storedRun) int64 {
	if r.RootID != nil {
		return *r.RootID
	}
	return r.ID
}
