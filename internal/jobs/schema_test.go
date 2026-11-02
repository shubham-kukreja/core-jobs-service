package jobs

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"cloud.google.com/go/civil"
)

type schemaArgs struct {
	Model  string            `json:"model"`
	AsOf   civil.Date        `json:"as_of"`
	Cutoff *time.Time        `json:"cutoff,omitempty"`
	Series []string          `json:"series,omitempty"`
	Params map[string]string `json:"params,omitempty"`
	Count  int               `json:"count"`
	Ratio  float64           `json:"ratio,omitempty"`
	Pub    bool              `json:"publish"`
	Hidden string            `json:"-"`
	inner  int               //nolint:unused // verifies unexported fields are skipped.
}

func (schemaArgs) Kind() string { return "test.schema" }

func TestGenerateArgsSchema(t *testing.T) {
	var got map[string]any
	if err := json.Unmarshal(GenerateArgsSchema(reflect.TypeFor[schemaArgs]()), &got); err != nil {
		t.Fatal(err)
	}
	if got["additionalProperties"] != false || got["type"] != "object" {
		t.Fatalf("top level: %v", got)
	}
	p := got["properties"].(map[string]any)
	want := map[string]map[string]any{
		"model":   {"type": "string"},
		"as_of":   {"type": "string", "format": "date"},
		"cutoff":  {"type": "string", "format": "date-time"},
		"count":   {"type": "integer"},
		"ratio":   {"type": "number"},
		"publish": {"type": "boolean"},
	}
	for name, w := range want {
		for k, v := range w {
			if p[name].(map[string]any)[k] != v {
				t.Errorf("%s.%s = %v, want %v", name, k, p[name].(map[string]any)[k], v)
			}
		}
	}
	if p["series"].(map[string]any)["type"] != "array" || p["params"].(map[string]any)["type"] != "object" {
		t.Errorf("collections: %v %v", p["series"], p["params"])
	}
	if _, ok := p["Hidden"]; ok {
		t.Error("json:\"-\" field must be omitted")
	}
	if _, ok := p["inner"]; ok {
		t.Error("unexported field must be omitted")
	}
	req := map[string]bool{}
	for _, r := range got["required"].([]any) {
		req[r.(string)] = true
	}
	if !req["model"] || !req["as_of"] || !req["count"] || !req["publish"] || req["cutoff"] || req["series"] || req["ratio"] {
		t.Errorf("required = %v", got["required"])
	}
}

func TestRegisterFillsSchema(t *testing.T) {
	r := NewRegistry()
	Register(r, pingSpec(), HandlerFunc[pingArgs](func(_ context.Context, _ *RunCtx, _ pingArgs) (Outcome, error) {
		return Outcome{}, nil
	}))
	k, _ := r.Get("test.ping")
	if len(k.Spec.ArgsSchema) == 0 {
		t.Fatal("schema not generated")
	}
}
