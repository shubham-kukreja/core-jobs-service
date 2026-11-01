package seed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShippedSeedIsValid(t *testing.T) {
	if _, err := Validate(filepath.Join("..", "..", "configs", "seed")); err != nil {
		t.Fatal(err)
	}
}

func TestRejects(t *testing.T) {
	cases := map[string]struct {
		file, body, want string
	}{
		"secret in source": {"sources.yaml", `- {key: fred, kind: external_api, name: FRED, config: {api_key: abc}}`, "sources.yaml"},
		"bad key":          {"entities.yaml", `- {key: "Equity Large", kind: segment, name: X}`, "entities.yaml"},
		"score without derivation": {"series.yaml",
			`- {key: "x::y", name: X, layer: score, value_type: num, frequency: monthly}`, "series.yaml"},
		"unknown input": {"series.yaml",
			`- {key: a, name: A, layer: derived, value_type: num, frequency: monthly, derivation: {fn: expr, inputs: {x: missing}}}`, "unknown series"},
		"cycle": {"series.yaml", `
- {key: a, name: A, layer: derived, value_type: num, frequency: monthly, derivation: {fn: expr, inputs: {x: b}}}
- {key: b, name: B, layer: derived, value_type: num, frequency: monthly, derivation: {fn: expr, inputs: {x: a}}}`, "cycle"},
		"weights": {"risk_profiles.yaml", `
- {key: p, name: P, neutral_weights: {l1.equity: 0.5, l1.debt: 0.4}}`, "sum to 0.9"},
		"http base url": {"sources.yaml", `- {key: x, kind: external_api, name: X, config: {base_url: "http://x.com"}}`, "sources.yaml"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "entities.yaml", "- {key: l1.equity, kind: asset_class, name: Equity}\n- {key: l1.debt, kind: asset_class, name: Debt}\n")
			write(t, dir, tc.file, tc.body)
			_, err := Validate(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
