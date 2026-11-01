package jobs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
)

// fakeStore records what the executor persists.
type fakeStore struct {
	mu        sync.Mutex
	run       storedRun
	runnable  bool
	cancelReq atomic.Bool
	status    Status
	runErr    *RunError
	completed *Outcome
	requeued  []string
	writes    []int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		run:      storedRun{ID: 1, UUID: uuid.New(), Kind: "test.exec", Args: []byte(`{}`), Trigger: TriggerUser, Status: StatusQueued},
		runnable: true,
	}
}

func (f *fakeStore) get(context.Context, int64) (storedRun, error) { return f.run, nil }
func (f *fakeStore) markRunning(context.Context, int64, int, time.Time) (bool, error) {
	return f.runnable, nil
}
func (f *fakeStore) cancelRequested(context.Context, int64) (bool, error) {
	return f.cancelReq.Load(), nil
}
func (f *fakeStore) progress(_ context.Context, _ int64, pct int, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, pct)
	return nil
}
func (f *fakeStore) complete(_ context.Context, _ storedRun, out Outcome, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.completed = StatusSucceeded, &out
	return nil
}
func (f *fakeStore) fail(_ context.Context, _ int64, st Status, re RunError, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.runErr = st, &re
	return nil
}
func (f *fakeStore) requeue(_ context.Context, _ int64, msg string, re *RunError) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.runErr = StatusQueued, re
	f.requeued = append(f.requeued, msg)
	return nil
}

type execArgs struct{}

func (execArgs) Kind() string { return "test.exec" }

func execRegistry(timeout time.Duration, h HandlerFunc[execArgs]) *Registry {
	r := NewRegistry()
	Register(r, Spec{
		Kind: "test.exec", Queue: QueueSystem, Triggers: []TriggerType{TriggerUser},
		UserTriggerable: true, Roles: []string{"admin"},
		MaxAttempts: 3, Timeout: timeout, ArgsVersion: 1,
	}, h)
	return r
}

func run(t *testing.T, h HandlerFunc[execArgs], attempt int, opts ...func(*fakeStore)) (*fakeStore, error) {
	t.Helper()
	st := newFakeStore()
	for _, o := range opts {
		o(st)
	}
	ex := newExecutor(execRegistry(5*time.Second, h), st, ExecutorConfig{CancelPoll: 20 * time.Millisecond})
	err := ex.Execute(context.Background(), Execution{RunID: 1, Attempt: attempt, MaxAttempts: 3})
	return st, err
}

func TestExecuteSuccess(t *testing.T) {
	st, err := run(t, func(context.Context, *RunCtx, execArgs) (Outcome, error) {
		return Outcome{Summary: map[string]any{"rows": 3}}, nil
	}, 1)
	if err != nil || st.status != StatusSucceeded || st.completed.Summary["rows"] != 3 {
		t.Fatalf("err=%v status=%s out=%v", err, st.status, st.completed)
	}
}

func TestExecuteSkipsCancelledWhileQueued(t *testing.T) {
	called := false
	st, err := run(t, func(context.Context, *RunCtx, execArgs) (Outcome, error) {
		called = true
		return Outcome{}, nil
	}, 1, func(s *fakeStore) { s.runnable = false })
	if err != nil || called || st.status != "" {
		t.Fatalf("err=%v called=%v status=%s", err, called, st.status)
	}
}

func TestExecuteRetryThenFail(t *testing.T) {
	boom := errors.New("db connection reset")
	h := func(context.Context, *RunCtx, execArgs) (Outcome, error) { return Outcome{}, boom }

	st, err := run(t, h, 1)
	if IsPermanent(err) || !errors.Is(err, boom) || st.status != StatusQueued || !st.runErr.Retryable {
		t.Fatalf("attempt 1: err=%v status=%s re=%+v", err, st.status, st.runErr)
	}
	st, err = run(t, h, 3)
	if !IsPermanent(err) || st.status != StatusFailed || st.runErr.Retryable {
		t.Fatalf("last attempt: err=%v status=%s re=%+v", err, st.status, st.runErr)
	}
	if strings.Contains(st.runErr.Message, "connection reset") {
		t.Fatalf("internal cause leaked into stored error: %q", st.runErr.Message)
	}
}

func TestExecutePermanentAndAppErrors(t *testing.T) {
	st, err := run(t, func(context.Context, *RunCtx, execArgs) (Outcome, error) {
		return Outcome{}, Permanent(apperr.New(apperr.InvalidParams, "sectors held exceed max sectors"))
	}, 1)
	if !IsPermanent(err) || st.status != StatusFailed {
		t.Fatalf("err=%v status=%s", err, st.status)
	}
	if st.runErr.Code != string(apperr.InvalidParams) || st.runErr.Message != "sectors held exceed max sectors" {
		t.Fatalf("stored error = %+v", st.runErr)
	}
}

func TestExecutePanicIsPermanentWithoutStack(t *testing.T) {
	st, err := run(t, func(context.Context, *RunCtx, execArgs) (Outcome, error) {
		panic("secret=hunter2")
	}, 1)
	if !IsPermanent(err) || st.status != StatusFailed {
		t.Fatalf("err=%v status=%s", err, st.status)
	}
	if st.runErr.Message != "internal error" || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("panic details leaked: %+v / %v", st.runErr, err)
	}
}

func TestExecuteSnooze(t *testing.T) {
	st, err := run(t, func(context.Context, *RunCtx, execArgs) (Outcome, error) {
		return Outcome{}, Snooze(time.Hour)
	}, 1)
	if d, ok := SnoozeDuration(err); !ok || d != time.Hour || st.status != StatusQueued {
		t.Fatalf("err=%v status=%s", err, st.status)
	}
}

// Acceptance (P1-T2-03): cancel stops a sleeping job within 3s.
func TestExecuteCancelStopsSleepingJob(t *testing.T) {
	st := newFakeStore()
	h := func(ctx context.Context, _ *RunCtx, _ execArgs) (Outcome, error) {
		select {
		case <-ctx.Done():
			return Outcome{}, ctx.Err()
		case <-time.After(30 * time.Second):
			return Outcome{}, nil
		}
	}
	ex := newExecutor(execRegistry(time.Minute, h), st, ExecutorConfig{}) // default 2s poll
	go func() {
		time.Sleep(200 * time.Millisecond)
		st.cancelReq.Store(true)
	}()
	start := time.Now()
	err := ex.Execute(context.Background(), Execution{RunID: 1, Attempt: 1, MaxAttempts: 3})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cancel took %s", elapsed)
	}
	if err != nil || st.status != StatusCancelled {
		t.Fatalf("err=%v status=%s", err, st.status)
	}
}

func TestExecuteTimeout(t *testing.T) {
	st := newFakeStore()
	h := func(ctx context.Context, _ *RunCtx, _ execArgs) (Outcome, error) {
		<-ctx.Done()
		return Outcome{}, ctx.Err()
	}
	ex := newExecutor(execRegistry(50*time.Millisecond, h), st, ExecutorConfig{})
	err := ex.Execute(context.Background(), Execution{RunID: 1, Attempt: 3, MaxAttempts: 3})
	if !IsPermanent(err) || st.status != StatusFailed || st.runErr.Code != "timeout" {
		t.Fatalf("err=%v status=%s re=%+v", err, st.status, st.runErr)
	}
}

func TestExecuteShutdownRequeuesWithoutAttempt(t *testing.T) {
	st := newFakeStore()
	h := func(ctx context.Context, _ *RunCtx, _ execArgs) (Outcome, error) {
		<-ctx.Done()
		return Outcome{}, ctx.Err()
	}
	ex := newExecutor(execRegistry(time.Minute, h), st, ExecutorConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	err := ex.Execute(ctx, Execution{RunID: 1, Attempt: 1, MaxAttempts: 3})
	if d, ok := SnoozeDuration(err); !ok || d != 0 || st.status != StatusQueued {
		t.Fatalf("err=%v status=%s", err, st.status)
	}
}

func TestProgressThrottled(t *testing.T) {
	st, err := run(t, func(_ context.Context, rc *RunCtx, _ execArgs) (Outcome, error) {
		for i := range 50 {
			rc.Progress(i, "working")
		}
		rc.Progress(100, "done")
		return Outcome{}, nil
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	// First write goes through, the burst is throttled, 100% always lands.
	if len(st.writes) != 2 || st.writes[0] != 0 || st.writes[1] != 100 {
		t.Fatalf("progress writes = %v", st.writes)
	}
}

func TestUnknownKindFailsPermanently(t *testing.T) {
	st := newFakeStore()
	st.run.Kind = "nope"
	ex := newExecutor(NewRegistry(), st, ExecutorConfig{})
	err := ex.Execute(context.Background(), Execution{RunID: 1, Attempt: 1, MaxAttempts: 3})
	if !IsPermanent(err) || st.status != StatusFailed {
		t.Fatalf("err=%v status=%s", err, st.status)
	}
}
