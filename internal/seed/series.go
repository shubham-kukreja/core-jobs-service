package seed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
)

// seriesDoc is one entry of series.yaml, already checked against series.json.
type seriesDoc struct {
	Key            string              `json:"key"`
	Name           string              `json:"name"`
	Entity         string              `json:"entity"`
	Layer          catalog.Layer       `json:"layer"`
	ValueType      catalog.ValueType   `json:"value_type"`
	Unit           string              `json:"unit"`
	Frequency      catalog.Frequency   `json:"frequency"`
	Align          catalog.Align       `json:"align"`
	AllowedValues  []string            `json:"allowed_values"`
	Validation     catalog.Validation  `json:"validation"`
	Derivation     *catalog.Derivation `json:"derivation"`
	MinHistory     *int                `json:"min_history"`
	SourcePriority []string            `json:"source_priority"`
	Tags           []string            `json:"tags"`
	Meta           map[string]any      `json:"meta"`
	Registry       struct {
		DisplayName string   `json:"display_name"`
		Node        string   `json:"node"`
		Signal      string   `json:"signal"`
		Group       string   `json:"group"`
		UpdateType  string   `json:"update_type"`
		Owner       string   `json:"owner"`
		StaleAfter  string   `json:"stale_after"`
		Flags       []string `json:"flags"`
	} `json:"registry"`
}

// SeriesInputs converts series.yaml to catalog inputs ordered so every
// derivation's inputs come before it, which is the order SaveSeries needs.
// docs must have passed Validate.
func SeriesInputs(docs Documents) ([]catalog.SeriesInput, error) {
	raw, err := json.Marshal(docs["series.yaml"])
	if err != nil {
		return nil, err
	}
	var in []seriesDoc
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("series.yaml: %w", err)
	}
	byKey := make(map[string]catalog.SeriesInput, len(in))
	for _, d := range in {
		stale, err := parseStaleAfter(d.Registry.StaleAfter)
		if err != nil {
			return nil, fmt.Errorf("series.yaml: %s: registry.stale_after: %w", d.Key, err)
		}
		byKey[d.Key] = catalog.SeriesInput{
			Key: d.Key, Name: d.Name, EntityKey: d.Entity, Layer: d.Layer, ValueType: d.ValueType,
			Unit: d.Unit, Frequency: d.Frequency, Align: d.Align, AllowedValues: d.AllowedValues,
			Validation: d.Validation, Derivation: d.Derivation, MinHistory: d.MinHistory,
			SourcePriority: d.SourcePriority, Tags: d.Tags, Meta: d.Meta,
			Registry: catalog.RegistryMeta{
				DisplayName: d.Registry.DisplayName, NodeKey: d.Registry.Node, SignalKey: d.Registry.Signal,
				Group: d.Registry.Group, UpdateType: d.Registry.UpdateType, Owner: d.Registry.Owner,
				StaleAfter: stale, Flags: d.Registry.Flags,
			},
		}
	}
	return topoOrder(byKey)
}

// topoOrder returns inputs before dependents, ties broken by key so the
// order is stable. Validate has already rejected cycles and unknown inputs.
func topoOrder(byKey map[string]catalog.SeriesInput) ([]catalog.SeriesInput, error) {
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]catalog.SeriesInput, 0, len(keys))
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(k string) error
	visit = func(k string) error {
		switch state[k] {
		case 1:
			return fmt.Errorf("series.yaml: dependency cycle at %q", k)
		case 2:
			return nil
		}
		s, ok := byKey[k]
		if !ok {
			return fmt.Errorf("series.yaml: unknown series %q", k)
		}
		state[k] = 1
		if s.Derivation != nil {
			deps := make([]string, 0, len(s.Derivation.Inputs))
			for _, d := range s.Derivation.Inputs {
				deps = append(deps, d)
			}
			sort.Strings(deps)
			for _, d := range deps {
				if err := visit(d); err != nil {
					return err
				}
			}
		}
		state[k] = 2
		out = append(out, s)
		return nil
	}
	for _, k := range keys {
		if err := visit(k); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// parseStaleAfter reads the schema's "<n>h" or "<n>d".
func parseStaleAfter(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	unit := time.Hour
	switch {
	case strings.HasSuffix(s, "d"):
		unit = 24 * time.Hour
	case strings.HasSuffix(s, "h"):
	default:
		return 0, fmt.Errorf("want <n>h or <n>d, got %q", s)
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n < 0 {
		return 0, fmt.Errorf("want <n>h or <n>d, got %q", s)
	}
	return time.Duration(n) * unit, nil
}
