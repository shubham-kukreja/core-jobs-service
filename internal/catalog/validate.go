package catalog

import (
	"math"
	"regexp"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
)

// Patterns mirror the SQL CHECKs (platform.is_key) and configs/schema.
var (
	keyRE      = regexp.MustCompile(`^[a-z0-9][a-z0-9_.\-]{0,126}(::[a-z0-9][a-z0-9_\-]{0,62})?$`)
	tokenRE    = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	tagRE      = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
	fnRE       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	aliasRE    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	maxNameLen = 200
)

// ValidKey reports whether k is a well-formed catalog key.
func ValidKey(k string) bool { return keyRE.MatchString(k) }

var (
	layers      = set(LayerRaw, LayerJudgment, LayerDerived, LayerWindow, LayerPercentile, LayerScore, LayerTrigger, LayerVeto)
	valueTypes  = set(ValueNum, ValueBool, ValueCat)
	frequencies = set(FreqDaily, FreqWeekly, FreqMonthly, FreqQuarterly, FreqEvent)
	aligns      = set(AlignLast, AlignMonthEnd, AlignMean)
	entityKinds = set("asset_class", "segment", "bucket", "metal", "sector", "index", "macro")
	groups      = set("equity", "debt", "metals", "governance", "veto")
	updateTypes = set("derived", "rubric", "analyst", "constant", "governance", "veto", "raw")
	flags       = set("mock", "constant", "default")
)

func set[T comparable](xs ...T) map[T]bool {
	m := make(map[T]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

type problems []apperr.Detail

func (p *problems) add(field, reason string, meta map[string]any) {
	*p = append(*p, apperr.Detail{Field: field, Reason: reason, Meta: meta})
}

func (p problems) err(msg string) error {
	if len(p) == 0 {
		return nil
	}
	return apperr.New(apperr.ValidationFailed, msg, p...)
}

// normalizeSeries applies defaults and validates everything that does not
// need the database. References (entity, inputs, sources) are checked by the
// store.
func normalizeSeries(in *SeriesInput) error {
	var p problems
	if !ValidKey(in.Key) {
		p.add("key", "invalid_key", nil)
	}
	if l := len(in.Name); l == 0 || l > maxNameLen {
		p.add("name", "invalid_length", map[string]any{"max": maxNameLen})
	}
	if in.EntityKey != "" && !ValidKey(in.EntityKey) {
		p.add("entity", "invalid_key", nil)
	}
	if !layers[in.Layer] {
		p.add("layer", "unknown_layer", nil)
	}
	if !valueTypes[in.ValueType] {
		p.add("value_type", "unknown_value_type", nil)
	}
	if !frequencies[in.Frequency] {
		p.add("frequency", "unknown_frequency", nil)
	}
	if in.Align == "" {
		in.Align = AlignLast
	}
	if !aligns[in.Align] {
		p.add("align", "unknown_align", nil)
	}
	if len(in.Unit) > 32 {
		p.add("unit", "invalid_length", map[string]any{"max": 32})
	}

	// Layer ↔ value type.
	switch in.Layer {
	case LayerPercentile, LayerScore:
		if in.ValueType != ValueNum {
			p.add("value_type", "layer_requires_num", nil)
		}
	case LayerTrigger, LayerVeto:
		if in.ValueType != ValueBool {
			p.add("value_type", "layer_requires_bool", nil)
		}
	}

	// Categories.
	if in.ValueType == ValueCat {
		if len(in.AllowedValues) < 2 {
			p.add("allowed_values", "required_for_cat", map[string]any{"min": 2})
		}
		seen := map[string]bool{}
		for _, v := range in.AllowedValues {
			if !tokenRE.MatchString(v) || seen[v] {
				p.add("allowed_values", "invalid_value", map[string]any{"value": v})
			}
			seen[v] = true
		}
	} else if len(in.AllowedValues) > 0 {
		p.add("allowed_values", "only_for_cat", nil)
	}

	// Numeric bounds.
	v := in.Validation
	if (v.Min != nil || v.Max != nil) && in.ValueType != ValueNum {
		p.add("validation", "only_for_num", nil)
	}
	for _, b := range []*float64{v.Min, v.Max} {
		if b != nil && (math.IsNaN(*b) || math.IsInf(*b, 0)) {
			p.add("validation", "not_finite", nil)
		}
	}
	if v.Min != nil && v.Max != nil && *v.Min > *v.Max {
		p.add("validation", "min_above_max", nil)
	}

	// Derivation presence by layer (mirrors the SQL CHECKs).
	switch {
	case in.Layer.Computed() && in.Derivation == nil:
		p.add("derivation", "required_for_layer", nil)
	case (in.Layer == LayerRaw || in.Layer == LayerJudgment || in.Layer == LayerVeto) && in.Derivation != nil:
		p.add("derivation", "not_allowed_for_layer", nil)
	}
	if d := in.Derivation; d != nil {
		if !fnRE.MatchString(d.Fn) {
			p.add("derivation.fn", "invalid_fn", nil)
		}
		for alias, k := range d.Inputs {
			if !aliasRE.MatchString(alias) {
				p.add("derivation.inputs", "invalid_alias", map[string]any{"alias": alias})
			}
			if !ValidKey(k) {
				p.add("derivation.inputs", "invalid_key", map[string]any{"alias": alias})
			}
			if k == in.Key {
				p.add("derivation.inputs", "self_reference", map[string]any{"alias": alias})
			}
		}
	}

	if in.MinHistory != nil && *in.MinHistory < 1 {
		p.add("min_history", "must_be_positive", nil)
	}
	seenSrc := map[string]bool{}
	for _, s := range in.SourcePriority {
		if !ValidKey(s) || seenSrc[s] {
			p.add("source_priority", "invalid_or_duplicate", map[string]any{"value": s})
		}
		seenSrc[s] = true
	}
	for _, t := range in.Tags {
		if !tagRE.MatchString(t) {
			p.add("tags", "invalid_tag", map[string]any{"value": t})
		}
	}

	r := in.Registry
	if len(r.DisplayName) > maxNameLen {
		p.add("registry.display_name", "invalid_length", map[string]any{"max": maxNameLen})
	}
	if r.NodeKey != "" && !ValidKey(r.NodeKey) {
		p.add("registry.node", "invalid_key", nil)
	}
	if r.SignalKey != "" && !ValidKey(r.SignalKey) {
		p.add("registry.signal", "invalid_key", nil)
	}
	if r.Group != "" && !groups[r.Group] {
		p.add("registry.group", "unknown_group", nil)
	}
	if r.UpdateType != "" && !updateTypes[r.UpdateType] {
		p.add("registry.update_type", "unknown_update_type", nil)
	}
	if len(r.Owner) > 64 {
		p.add("registry.owner", "invalid_length", map[string]any{"max": 64})
	}
	if r.StaleAfter < 0 {
		p.add("registry.stale_after", "must_not_be_negative", nil)
	}
	for _, f := range r.Flags {
		if !flags[f] {
			p.add("registry.flags", "unknown_flag", map[string]any{"value": f})
		}
	}
	return p.err("series definition is invalid")
}

func validateEntity(e Entity) error {
	var p problems
	if !ValidKey(e.Key) {
		p.add("key", "invalid_key", nil)
	}
	if !entityKinds[e.Kind] {
		p.add("kind", "unknown_kind", nil)
	}
	if l := len(e.Name); l == 0 || l > maxNameLen {
		p.add("name", "invalid_length", map[string]any{"max": maxNameLen})
	}
	if e.ParentKey != "" && (!ValidKey(e.ParentKey) || e.ParentKey == e.Key) {
		p.add("parent", "invalid_parent", nil)
	}
	return p.err("entity is invalid")
}
