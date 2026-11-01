// Package seed validates (and, from Phase 1, loads) the YAML seed files in
// configs/seed: structure against the embedded JSON Schemas, then cross-file
// references and invariants the schemas cannot express.
package seed

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/shubham-kukreja/core-jobs-service/configs"
)

const schemaBase = "https://core.arviawealth.com/schema/"

// MaxFileBytes caps a seed file's size.
const MaxFileBytes = 5 << 20

// files maps each seed file (or glob) to the schema it must satisfy.
var files = []struct {
	glob   string
	schema string
}{
	{"entities.yaml", "entities.json"},
	{"sources.yaml", "sources.json"},
	{"series.yaml", "series.json"},
	{"risk_profiles.yaml", "risk_profiles.json"},
	{"schedules.yaml", "schedules.json"},
	{"model_*.yaml", "model_config.json"},
	{"import_mappings/*.yaml", "import_mapping_file.json"},
}

// Documents are the decoded seed files, keyed by path relative to the seed dir.
type Documents map[string]any

// Validate checks every seed file in dir. It returns all problems found,
// joined, so one run reports everything.
func Validate(dir string) (Documents, error) {
	schemas, err := compileSchemas()
	if err != nil {
		return nil, err
	}
	docs := Documents{}
	var errs []error
	for _, f := range files {
		paths, err := filepath.Glob(filepath.Join(dir, f.glob))
		if err != nil {
			return nil, err
		}
		for _, p := range paths {
			rel, _ := filepath.Rel(dir, p)
			doc, err := readYAML(p)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", rel, err))
				continue
			}
			if err := schemas[f.schema].Validate(doc); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", rel, err))
				continue
			}
			docs[rel] = doc
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if err := crossCheck(docs); err != nil {
		return nil, err
	}
	return docs, nil
}

func compileSchemas() (map[string]*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	entries, err := fs.ReadDir(configs.Schemas, "schema")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		b, err := fs.ReadFile(configs.Schemas, "schema/"+e.Name())
		if err != nil {
			return nil, err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("schema %s: %w", e.Name(), err)
		}
		if err := c.AddResource(schemaBase+e.Name(), doc); err != nil {
			return nil, fmt.Errorf("schema %s: %w", e.Name(), err)
		}
	}
	out := map[string]*jsonschema.Schema{}
	for _, f := range files {
		s, err := c.Compile(schemaBase + f.schema)
		if err != nil {
			return nil, fmt.Errorf("schema %s: %w", f.schema, err)
		}
		out[f.schema] = s
	}
	return out, nil
}

// readYAML decodes a YAML file into JSON-compatible values (the schema
// validator's input), rejecting oversized files and duplicate keys.
func readYAML(path string) (any, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Size() > MaxFileBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", MaxFileBytes)
	}
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied seed directory.
	if err != nil {
		return nil, err
	}
	var v any
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	// Round-trip through JSON so numbers, dates and maps have JSON types.
	j, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("not JSON-compatible: %w", err)
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(j))
}

// crossCheck validates references between files and numeric invariants.
func crossCheck(docs Documents) error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	entities := map[string]map[string]any{}
	for _, e := range list(docs["entities.yaml"]) {
		entities[str(e, "key")] = e
	}
	for k, e := range entities {
		if p := str(e, "parent"); p != "" {
			if _, ok := entities[p]; !ok {
				add("entities.yaml: %s: unknown parent %q", k, p)
			}
		}
	}

	series := map[string]map[string]any{}
	for _, s := range list(docs["series.yaml"]) {
		k := str(s, "key")
		if _, dup := series[k]; dup {
			add("series.yaml: duplicate key %q", k)
		}
		series[k] = s
	}
	for k, s := range series {
		if ent := str(s, "entity"); ent != "" {
			if _, ok := entities[ent]; !ok {
				add("series.yaml: %s: unknown entity %q", k, ent)
			}
		}
		if d, ok := s["derivation"].(map[string]any); ok {
			for alias, in := range obj(d["inputs"]) {
				if _, ok := series[fmt.Sprint(in)]; !ok {
					add("series.yaml: %s: input %s references unknown series %q", k, alias, in)
				}
			}
		}
	}
	if cyc := findCycle(series); cyc != nil {
		add("series.yaml: dependency cycle: %s", strings.Join(cyc, " → "))
	}

	for _, p := range list(docs["risk_profiles.yaml"]) {
		checkNeutralWeights(str(p, "key"), obj(p["neutral_weights"]), entities, add)
	}

	for path, doc := range docs {
		if !strings.HasPrefix(filepath.Base(path), "model_") {
			continue
		}
		cfg := obj(obj(doc)["config"])
		for name, sc := range obj(cfg["scorecards"]) {
			checkSignalWeights(path, "scorecards/"+name, obj(sc)["signals"], add)
			for _, b := range anyList(obj(sc)["buckets"]) {
				if _, ok := entities[fmt.Sprint(b)]; !ok {
					add("%s: scorecards/%s: unknown bucket %q", path, name, b)
				}
			}
		}
		if sat := obj(cfg["sector_satellite"]); sat != nil {
			checkSignalWeights(path, "sector_satellite", sat["signals"], add)
			for _, s := range anyList(sat["sectors"]) {
				if _, ok := entities[fmt.Sprint(s)]; !ok {
					add("%s: sector_satellite: unknown sector %q", path, s)
				}
			}
		}
	}

	for path, doc := range docs {
		if !strings.HasPrefix(path, "import_mappings"+string(filepath.Separator)) {
			continue
		}
		for _, c := range anyList(obj(obj(doc)["mapping"])["columns"]) {
			s := str(obj(c), "series")
			def, ok := series[s]
			switch {
			case !ok:
				add("%s: column %q maps to unknown series %q", path, str(obj(c), "name"), s)
			case str(def, "layer") != "raw":
				add("%s: column %q maps to %s series %q; only raw series are imported", path, str(obj(c), "name"), str(def, "layer"), s)
			}
		}
	}

	if len(errs) > 0 {
		sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
		return errors.Join(errs...)
	}
	return nil
}

const weightTolerance = 1e-9

func checkSignalWeights(path, where string, signals any, add func(string, ...any)) {
	sum := 0.0
	for _, s := range anyList(signals) {
		sum += num(obj(s)["weight"])
	}
	if math.Abs(sum-1) > weightTolerance {
		add("%s: %s: signal weights sum to %g, want 1", path, where, sum)
	}
}

// checkNeutralWeights requires top-level nodes to sum to 1 and each parent's
// children (present in the profile) to sum to 1.
func checkNeutralWeights(profile string, w map[string]any, entities map[string]map[string]any, add func(string, ...any)) {
	groups := map[string]float64{}
	for k, v := range w {
		e, ok := entities[k]
		if !ok {
			add("risk_profiles.yaml: %s: unknown node %q", profile, k)
			continue
		}
		groups[str(e, "parent")] += num(v)
	}
	for parent, sum := range groups {
		if math.Abs(sum-1) > weightTolerance {
			name := parent
			if name == "" {
				name = "top level"
			}
			add("risk_profiles.yaml: %s: weights under %s sum to %g, want 1", profile, name, sum)
		}
	}
}

func findCycle(series map[string]map[string]any) []string {
	const (
		unvisited = iota
		visiting
		done
	)
	state := map[string]int{}
	var stack []string
	var visit func(k string) []string
	visit = func(k string) []string {
		switch state[k] {
		case visiting:
			for i, s := range stack {
				if s == k {
					return append(append([]string{}, stack[i:]...), k)
				}
			}
		case done:
			return nil
		}
		state[k] = visiting
		stack = append(stack, k)
		d, _ := series[k]["derivation"].(map[string]any)
		inputs := make([]string, 0)
		for _, in := range obj(d["inputs"]) {
			inputs = append(inputs, fmt.Sprint(in))
		}
		sort.Strings(inputs)
		for _, in := range inputs {
			if _, ok := series[in]; ok {
				if c := visit(in); c != nil {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[k] = done
		return nil
	}
	keys := make([]string, 0, len(series))
	for k := range series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if c := visit(k); c != nil {
			return c
		}
	}
	return nil
}

// Helpers over schema-validated JSON values.

func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func anyList(v any) []any      { l, _ := v.([]any); return l }
func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func list(v any) []map[string]any {
	var out []map[string]any
	for _, x := range anyList(v) {
		if m := obj(x); m != nil {
			out = append(out, m)
		}
	}
	return out
}

func num(v any) float64 {
	switch n := v.(type) {
	case json.Number:
		f, _ := n.Float64()
		return f
	case float64:
		return n
	}
	return math.NaN()
}
