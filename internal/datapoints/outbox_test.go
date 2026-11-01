package datapoints

import "testing"

func TestFindSensitiveKey(t *testing.T) {
	cases := map[string]struct {
		req  any
		want string
	}{
		"clean":     {map[string]any{"series_id": "DFII10", "series_key": "x", "params": map[string]any{"from": "2026-01-01"}}, ""},
		"api key":   {map[string]any{"api_key": "abc"}, "api_key"},
		"bare key":  {map[string]any{"Key": "abc"}, "Key"},
		"nested":    {map[string]any{"headers": map[string]any{"Authorization": "Bearer x"}}, "Authorization"},
		"in list":   {map[string]any{"h": []any{map[string]any{"X-Auth-Token": "x"}}}, "X-Auth-Token"},
		"signature": {map[string]any{"query": map[string]any{"X-Amz-Signature": "x"}}, "X-Amz-Signature"},
	}
	for name, c := range cases {
		if got := findSensitiveKey(c.req); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}

func TestInt64Array(t *testing.T) {
	if got := int64Array([]int64{3, 1, 22}); got != "{3,1,22}" {
		t.Fatalf("got %q", got)
	}
	if got := int64Array(nil); got != "{}" {
		t.Fatalf("got %q", got)
	}
}
