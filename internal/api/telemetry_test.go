package api

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

type obs struct {
	method, route string
	code          int
}

type recMetrics struct {
	mu  sync.Mutex
	got []obs
}

func (m *recMetrics) ObserveHTTP(method, route string, code int, _ time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.got = append(m.got, obs{method, route, code})
}

// Metrics are labelled with the route pattern, never the raw path, so ids
// never become label values.
func TestHTTPMetricsUseRoutePattern(t *testing.T) {
	m := &recMetrics{}
	h := newHarness(t, func(_ *Config, d *Deps) { d.Metrics = m })
	h.do(req{method: http.MethodGet, path: "/v1/runs/7f1c2c4e-1d5a-4c1e-9a7b-2b3c4d5e6f70"})
	h.as(viewerToken, req{method: http.MethodGet, path: "/v1/me"})
	h.do(req{method: http.MethodGet, path: "/v1/no/such/thing"})
	want := []obs{
		{http.MethodGet, "/v1/runs/{id}", http.StatusUnauthorized},
		{http.MethodGet, "/v1/me", http.StatusOK},
		{http.MethodGet, "unmatched", http.StatusNotFound},
	}
	if len(m.got) != len(want) {
		t.Fatalf("observations = %+v", m.got)
	}
	for i := range want {
		if m.got[i] != want[i] {
			t.Errorf("observation %d = %+v, want %+v", i, m.got[i], want[i])
		}
	}
}
