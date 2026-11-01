package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type pingArgs struct {
	N int `json:"n"`
}

func (pingArgs) Kind() string { return "test.ping" }

func pingSpec() Spec {
	return Spec{
		Kind: "test.ping", Queue: QueueSystem, Triggers: []TriggerType{TriggerUser},
		UserTriggerable: true, Roles: []string{"admin"},
		MaxAttempts: 1, Timeout: time.Second, ArgsVersion: 1,
	}
}

func TestRegisterAndInvoke(t *testing.T) {
	r := NewRegistry()
	Register(r, pingSpec(), HandlerFunc[pingArgs](func(_ context.Context, _ *RunCtx, a pingArgs) (Outcome, error) {
		return Outcome{Summary: map[string]any{"n": a.N}}, nil
	}))
	k, ok := r.Get("test.ping")
	if !ok {
		t.Fatal("kind not registered")
	}
	out, err := k.Invoke(context.Background(), &RunCtx{}, json.RawMessage(`{"n":3}`))
	if err != nil || out.Summary["n"] != 3 {
		t.Fatalf("got %v, %v", out, err)
	}
}

func TestInvokeRejectsUnknownFields(t *testing.T) {
	r := NewRegistry()
	Register(r, pingSpec(), HandlerFunc[pingArgs](func(context.Context, *RunCtx, pingArgs) (Outcome, error) {
		return Outcome{}, nil
	}))
	k, _ := r.Get("test.ping")
	_, err := k.Invoke(context.Background(), &RunCtx{}, json.RawMessage(`{"m":1}`))
	if !IsPermanent(err) {
		t.Fatalf("want permanent error, got %v", err)
	}
}

func TestRegisterPanics(t *testing.T) {
	noop := HandlerFunc[pingArgs](func(context.Context, *RunCtx, pingArgs) (Outcome, error) { return Outcome{}, nil })
	cases := map[string]func(*Registry){
		"duplicate": func(r *Registry) { Register(r, pingSpec(), noop); Register(r, pingSpec(), noop) },
		"kind mismatch": func(r *Registry) {
			s := pingSpec()
			s.Kind = "other"
			Register(r, s, noop)
		},
		"user without roles": func(r *Registry) {
			s := pingSpec()
			s.Roles = nil
			Register(r, s, noop)
		},
		"incomplete": func(r *Registry) {
			s := pingSpec()
			s.Timeout = 0
			Register(r, s, noop)
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			fn(NewRegistry())
		})
	}
}

func TestErrorHelpers(t *testing.T) {
	base := errors.New("boom")
	if !IsPermanent(Permanent(base)) || IsPermanent(Retryable(base)) || IsPermanent(base) {
		t.Fatal("IsPermanent misclassifies")
	}
	if !errors.Is(Permanent(base), base) {
		t.Fatal("Permanent must wrap its cause")
	}
	if d, ok := SnoozeDuration(Snooze(time.Minute)); !ok || d != time.Minute {
		t.Fatal("snooze not detected")
	}
}
