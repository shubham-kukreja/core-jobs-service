package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
)

func listSeriesOK(h *harness) {
	h.cat.ListSeriesFunc = func(context.Context, catalog.SeriesFilter) (paging.Page[catalog.Series], error) {
		return paging.Page[catalog.Series]{}, nil
	}
}

func TestAuthenticationRequired(t *testing.T) {
	h := newHarness(t)
	wantCode(t, h.do(req{method: "GET", path: "/v1/me", op: "/v1/me"}), 401, "unauthorized")
	wantCode(t, h.do(req{method: "GET", path: "/v1/me", op: "/v1/me", token: "bogus"}), 401, "unauthorized")
	wantCode(t, h.do(req{method: "GET", path: "/v1/me", op: "/v1/me", bearer: "core_bad"}), 401, "unauthorized")
	wantCode(t, h.do(req{method: "GET", path: "/v1/me", op: "/v1/me", headers: map[string]string{"Authorization": "Basic abc"}}), 401, "unauthorized")
	// Both credentials at once are refused, not guessed.
	wantCode(t, h.do(req{method: "GET", path: "/v1/me", op: "/v1/me", token: adminToken, bearer: adminKey}), 401, "unauthorized")

	rec := h.do(req{method: "GET", path: "/v1/me", op: "/v1/me", token: viewerToken})
	wantStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), viewerUser.Email) {
		t.Fatal(rec.Body.String())
	}
	wantStatus(t, h.do(req{method: "GET", path: "/v1/me", op: "/v1/me", bearer: adminKey}), 200)
}

func TestRoles(t *testing.T) {
	h := newHarness(t)
	body := `{"key":"sector.x","kind":"sector","name":"X"}`
	wantCode(t, h.as(viewerToken, req{method: "POST", path: "/v1/entities", op: "/v1/entities", body: body}), 403, "forbidden")
	wantCode(t, h.as(analystToken, req{method: "POST", path: "/v1/entities", op: "/v1/entities", body: body}), 403, "forbidden")
	wantCode(t, h.as(viewerToken, req{method: "POST", path: "/v1/datapoints", op: "/v1/datapoints",
		body: `{"source":"analyst","points":[{"series":"a","obs_date":"2026-10-31","value":1}]}`}), 403, "forbidden")
	if h.db.commits != 0 {
		t.Fatal("forbidden request reached a transaction")
	}
}

func TestCSRF(t *testing.T) {
	h := newHarness(t)
	listSeriesOK(h)
	body := `{"as_of":"2026-10-31","series":["x.*"]}`
	h.reader.SnapshotFunc = nil
	// Cookie auth + unsafe method without or with a wrong token: rejected.
	wantCode(t, h.do(req{method: "POST", path: "/v1/snapshot", op: "/v1/snapshot", body: body, token: viewerToken}), 403, "forbidden")
	wantCode(t, h.do(req{method: "POST", path: "/v1/snapshot", op: "/v1/snapshot", body: body, token: viewerToken,
		csrf: "csrfTokenBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"}), 403, "forbidden")
	// Safe methods need no token.
	wantStatus(t, h.do(req{method: "GET", path: "/v1/series", op: "/v1/series", token: viewerToken}), 200)
	// Cross-site browser requests are rejected even with a valid token.
	wantCode(t, h.do(req{method: "POST", path: "/v1/snapshot", op: "/v1/snapshot", body: body, token: viewerToken, csrf: csrfToken,
		headers: map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}}), 403, "forbidden")
	// The trusted frontend origin passes.
	h.reader.SnapshotFunc = snapshotEmpty
	wantStatus(t, h.do(req{method: "POST", path: "/v1/snapshot", op: "/v1/snapshot", body: body, token: viewerToken, csrf: csrfToken,
		headers: map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://core.arviawealth.com"}}), 200)
	// API keys need no CSRF token.
	wantStatus(t, h.do(req{method: "POST", path: "/v1/snapshot", op: "/v1/snapshot", body: body, bearer: adminKey}), 200)
}

func TestBodyLimitsAndStrictJSON(t *testing.T) {
	h := newHarness(t)
	big := `{"source":"analyst","points":[{"series":"a","obs_date":"2026-10-31","value":"` + strings.Repeat("x", 1<<16) + `"}]}`
	wantCode(t, h.as(analystToken, req{method: "POST", path: "/v1/datapoints", op: "/v1/datapoints", body: big}), 413, "payload_too_large")
	wantCode(t, h.as(adminToken, req{method: "POST", path: "/v1/entities", op: "/v1/entities",
		body: `{"key":"sector.x","kind":"sector","name":"X","is_admin":true}`}), 400, "validation_failed")
	wantCode(t, h.as(adminToken, req{method: "POST", path: "/v1/entities", op: "/v1/entities",
		body: `{"key":"sector.x","kind":"sector","name":"X"}{"key":"y"}`}), 400, "validation_failed")
	wantCode(t, h.as(adminToken, req{method: "POST", path: "/v1/entities", op: "/v1/entities", body: `{"key":`}), 400, "validation_failed")
	wantCode(t, h.as(adminToken, req{method: "POST", path: "/v1/entities", op: "/v1/entities",
		body: `{"key":"Bad Key","kind":"planet","name":""}`}), 400, "validation_failed")
}

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t)
	rec := h.do(req{method: "GET", path: "/v1/me", op: "/v1/me", token: viewerToken})
	for k, v := range map[string]string{
		"Content-Security-Policy":   "default-src 'none'; frame-ancestors 'none'",
		"X-Content-Type-Options":    "nosniff",
		"Referrer-Policy":           "no-referrer",
		"Cache-Control":             "no-store",
		"Strict-Transport-Security": "max-age=63072000; includeSubDomains",
	} {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if !strings.HasPrefix(rec.Header().Get("X-Request-Id"), "req_") {
		t.Fatal("missing request id")
	}
	// Inbound ids are accepted only in a strict shape.
	rec = h.do(req{method: "GET", path: "/v1/me", token: viewerToken, headers: map[string]string{"X-Request-Id": "abc\nINJECTED"}})
	if strings.Contains(rec.Header().Get("X-Request-Id"), "INJECTED") {
		t.Fatal("unsafe inbound request id echoed")
	}
}

func TestCORS(t *testing.T) {
	h := newHarness(t)
	pre := func(origin string) *http.Response {
		rec := h.do(req{method: "OPTIONS", path: "/v1/entities", headers: map[string]string{
			"Origin": origin, "Access-Control-Request-Method": "POST",
		}})
		return rec.Result()
	}
	ok := pre("https://core.arviawealth.com")
	if ok.StatusCode != 204 || ok.Header.Get("Access-Control-Allow-Origin") != "https://core.arviawealth.com" ||
		ok.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("allowed preflight: %d %v", ok.StatusCode, ok.Header)
	}
	bad := pre("https://evil.example")
	if bad.StatusCode != 403 || bad.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("disallowed preflight: %d %v", bad.StatusCode, bad.Header)
	}
}

func TestRateLimits(t *testing.T) {
	h := newHarness(t, func(c *Config, _ *Deps) { c.UserRate = Rate{Burst: 2, Per: time.Minute} })
	for range 2 {
		wantStatus(t, h.do(req{method: "GET", path: "/v1/me", op: "/v1/me", token: viewerToken}), 200)
	}
	rec := h.do(req{method: "GET", path: "/v1/me", token: viewerToken})
	wantCode(t, rec, 429, "rate_limited")
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
	h.clock.Advance(time.Minute)
	wantStatus(t, h.do(req{method: "GET", path: "/v1/me", token: viewerToken}), 200)
}

func TestTrustedProxyIP(t *testing.T) {
	h := newHarness(t)
	var seen []string
	h.srv.ipLimiter = limiterFunc(func(k string) (bool, time.Duration) { seen = append(seen, k); return true, 0 })
	h.h = h.srv.routes()
	r := req{method: "GET", path: "/v1/me", token: viewerToken, headers: map[string]string{"X-Forwarded-For": "198.51.100.1"}}
	h.do(r) // peer 203.0.113.9 is not a trusted proxy: header ignored
	if seen[0] != "ip:203.0.113.9" {
		t.Fatalf("spoofed XFF honoured: %v", seen)
	}
}

type limiterFunc func(string) (bool, time.Duration)

func (f limiterFunc) Allow(k string) (bool, time.Duration) { return f(k) }

func TestCursorTamperingRejected(t *testing.T) {
	h := newHarness(t)
	h.cat.ListSeriesFunc = func(_ context.Context, f catalog.SeriesFilter) (paging.Page[catalog.Series], error) {
		if f.Page.After == "" {
			return paging.Page[catalog.Series]{Items: []catalog.Series{testSeries()}, Next: "pe.nifty100"}, nil
		}
		if f.Page.After != "pe.nifty100" {
			t.Fatalf("forged position reached the repository: %q", f.Page.After)
		}
		return paging.Page[catalog.Series]{}, nil
	}
	rec := h.do(req{method: "GET", path: "/v1/series", op: "/v1/series", token: viewerToken})
	wantStatus(t, rec, 200)
	cursor := extract(t, rec.Body.String(), `"next_cursor":"`, `"`)
	wantStatus(t, h.do(req{method: "GET", path: "/v1/series?cursor=" + cursor, op: "/v1/series", token: viewerToken}), 200)

	forged := []byte(cursor)
	forged[len(forged)/2] ^= 1
	wantCode(t, h.do(req{method: "GET", path: "/v1/series?cursor=" + string(forged), op: "/v1/series", token: viewerToken}), 400, "invalid_params")
	// A cursor minted for another endpoint does not verify here.
	other := *h.srv.cursors.Encode("GET /v1/audit", "pe.nifty100")
	wantCode(t, h.do(req{method: "GET", path: "/v1/series?cursor=" + other, op: "/v1/series", token: viewerToken}), 400, "invalid_params")
}

func TestPanicIsGeneric500(t *testing.T) {
	h := newHarness(t)
	h.cat.ListEntitiesFunc = func(context.Context, string) ([]catalog.Entity, error) { panic("db password=hunter2 leaked") }
	rec := h.do(req{method: "GET", path: "/v1/entities", op: "/v1/entities", token: viewerToken})
	wantCode(t, rec, 500, "internal")
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatal("panic detail leaked to client")
	}
}

func TestSecretsNeverLogged(t *testing.T) {
	h := newHarness(t)
	h.do(req{method: "GET", path: "/v1/me?token=query-secret", token: viewerToken, csrf: csrfToken, bearer: ""})
	h.do(req{method: "GET", path: "/v1/me", bearer: adminKey})
	logs := h.logs.String()
	for _, secret := range []string{viewerToken, csrfToken, adminKey, "query-secret"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("secret %q in logs: %s", secret, logs)
		}
	}
	if !strings.Contains(logs, `"path":"/v1/me"`) {
		t.Fatalf("access log missing: %s", logs)
	}
}

func TestUnknownRoute(t *testing.T) {
	h := newHarness(t)
	wantCode(t, h.do(req{method: "GET", path: "/v1/nope", token: viewerToken}), 404, "not_found")
}

func extract(t *testing.T, s, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("%q not in %s", start, s)
	}
	s = s[i+len(start):]
	return s[:strings.Index(s, end)]
}
