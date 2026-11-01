package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"sync"
)

// Kind is a registered job kind with its type-erased handler.
type Kind struct {
	Spec   Spec
	invoke func(ctx context.Context, rc *RunCtx, raw json.RawMessage) (Outcome, error)
	// newArgs returns a pointer to a zero Args value, for decoding and schema generation.
	newArgs func() any
}

// Invoke decodes raw into the kind's Args and runs the handler. Unknown
// fields are rejected so a typo in a schedule's args fails loudly.
func (k Kind) Invoke(ctx context.Context, rc *RunCtx, raw json.RawMessage) (Outcome, error) {
	return k.invoke(ctx, rc, raw)
}

// ArgsType returns the Go type of the kind's Args.
func (k Kind) ArgsType() reflect.Type { return reflect.TypeOf(k.newArgs()).Elem() }

// Registry holds job kinds. Register every kind at startup, before the
// worker starts; the registry is read-only afterwards.
type Registry struct {
	mu    sync.RWMutex
	kinds map[string]Kind
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{kinds: map[string]Kind{}} }

// Register adds a kind. It panics on a duplicate kind or an inconsistent
// spec: both are programming errors that must fail at boot.
func Register[A Args](r *Registry, s Spec, h Handler[A]) {
	var zero A
	if s.Kind == "" || s.Kind != zero.Kind() {
		panic(fmt.Sprintf("jobs: spec kind %q does not match args kind %q", s.Kind, zero.Kind()))
	}
	if s.Queue == "" || len(s.Triggers) == 0 || s.ArgsVersion < 1 || s.MaxAttempts < 1 || s.Timeout <= 0 {
		panic(fmt.Sprintf("jobs: incomplete spec for %q", s.Kind))
	}
	if s.UserTriggerable && (!s.Allows(TriggerUser) || len(s.Roles) == 0) {
		panic(fmt.Sprintf("jobs: user-triggerable %q needs the user trigger and at least one role", s.Kind))
	}
	k := Kind{
		Spec:    s,
		newArgs: func() any { return new(A) },
		invoke: func(ctx context.Context, rc *RunCtx, raw json.RawMessage) (Outcome, error) {
			a, err := DecodeArgs[A](raw)
			if err != nil {
				return Outcome{}, Permanent(err)
			}
			return h.Handle(ctx, rc, a)
		},
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.kinds[s.Kind]; dup {
		panic(fmt.Sprintf("jobs: duplicate kind %q", s.Kind))
	}
	r.kinds[s.Kind] = k
}

// Get returns a kind by name.
func (r *Registry) Get(kind string) (Kind, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	k, ok := r.kinds[kind]
	return k, ok
}

// All returns every kind sorted by name.
func (r *Registry) All() []Kind {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Kind, 0, len(r.kinds))
	for _, k := range r.kinds {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Kind < out[j].Spec.Kind })
	return out
}

// DecodeArgs strictly decodes raw into A.
func DecodeArgs[A Args](raw json.RawMessage) (A, error) {
	var a A
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return a, fmt.Errorf("jobs: invalid args for %s: %w", a.Kind(), err)
	}
	return a, nil
}
