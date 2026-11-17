package derive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"sync"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
)

// InputsValidator is optionally implemented by primitives whose valid
// params depend on the derivation's aliases (expr variables, fallback order).
type InputsValidator interface {
	ValidateInputs(p json.RawMessage, aliases []string) error
}

type registry struct {
	mu sync.RWMutex
	m  map[string]Primitive
}

// NewRegistry returns an empty registry.
func NewRegistry() Registry { return &registry{m: map[string]Primitive{}} }

// NewDefaultRegistry returns a registry holding every built-in primitive.
func NewDefaultRegistry() Registry {
	r := NewRegistry()
	for _, p := range Builtins() {
		r.Register(p)
	}
	return r
}

// Register adds p. A duplicate name is a programming error and panics at boot.
func (r *registry) Register(p Primitive) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.m[p.Name()]; dup {
		panic("derive: duplicate primitive " + p.Name())
	}
	r.m[p.Name()] = p
}

func (r *registry) Get(name string) (Primitive, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.m[name]
	return p, ok
}

func (r *registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.m))
	for n := range r.m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ValidateDerivation checks fn, params and aliases and returns the sorted,
// de-duplicated series keys the derivation reads.
func (r *registry) ValidateDerivation(d catalog.Derivation) ([]string, error) {
	p, ok := r.Get(d.Fn)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownPrimitive, d.Fn)
	}
	if d.Version < 1 {
		return nil, fmt.Errorf("%w: version must be >= 1", ErrInvalidParams)
	}
	if err := p.ValidateParams(d.Params); err != nil {
		return nil, err
	}
	aliases := make([]string, 0, len(d.Inputs))
	keys := make([]string, 0, len(d.Inputs))
	for a, k := range d.Inputs {
		if a == "" || k == "" {
			return nil, fmt.Errorf("%w: empty input alias or series key", ErrInvalidParams)
		}
		aliases = append(aliases, a)
		keys = append(keys, k)
	}
	sort.Strings(aliases)
	if want := p.Aliases(d.Params); want != nil {
		w := slices.Clone(want)
		sort.Strings(w)
		if !slices.Equal(w, aliases) {
			return nil, fmt.Errorf("%w: %s needs inputs %v, got %v", ErrInvalidParams, d.Fn, w, aliases)
		}
	}
	if iv, ok := p.(InputsValidator); ok {
		if err := iv.ValidateInputs(d.Params, aliases); err != nil {
			return nil, err
		}
	}
	sort.Strings(keys)
	return slices.Compact(keys), nil
}

// decodeParams strictly decodes p into v; empty params decode as {}.
func decodeParams(p json.RawMessage, v any) error {
	if len(bytes.TrimSpace(p)) == 0 || string(bytes.TrimSpace(p)) == "null" {
		p = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(p))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidParams, err)
	}
	return nil
}
