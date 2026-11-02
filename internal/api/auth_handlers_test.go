package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shubham-kukreja/core-jobs-service/internal/auth"
)

const (
	issuedToken = "issuedTokenAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	issuedCSRF  = "issuedCsrfAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

func loginOK(f *fakeAuth, failures map[string]int) {
	f.login = func(email, pw string) (auth.Session, error) {
		if email == analystUser.Email && pw == "right-password" {
			return auth.Session{Token: issuedToken, CSRFToken: issuedCSRF, User: analystUser, ExpiresAt: testNow.Add(12 * time.Hour)}, nil
		}
		if failures != nil {
			failures[email]++
		}
		if email == "locked@arviawealth.com" {
			return auth.Session{}, auth.ErrLocked
		}
		return auth.Session{}, auth.ErrInvalidCredentials
	}
}

func login(h *harness, email, pw string, extra ...func(*req)) *http.Response {
	h.t.Helper()
	r := req{method: "POST", path: "/v1/auth/login", op: "/v1/auth/login", body: `{"email":"` + email + `","password":"` + pw + `"}`}
	for _, e := range extra {
		e(&r)
	}
	return h.do(r).Result()
}

func TestLoginSetsHardenedCookies(t *testing.T) {
	h := newHarness(t)
	loginOK(h.auth, nil)
	res := login(h, analystUser.Email, "right-password")
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	cookies := map[string]*http.Cookie{}
	for _, c := range res.Cookies() {
		cookies[c.Name] = c
	}
	s, c := cookies[sessionCookie], cookies[csrfCookie]
	if s == nil || s.Value != issuedToken || !s.HttpOnly || !s.Secure || s.SameSite != http.SameSiteStrictMode || s.Path != "/" {
		t.Fatalf("session cookie: %+v", s)
	}
	if c == nil || c.Value != issuedCSRF || c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("csrf cookie: %+v", c)
	}
}

func TestLoginRotatesExistingSession(t *testing.T) {
	h := newHarness(t)
	loginOK(h.auth, nil)
	login(h, analystUser.Email, "right-password", func(r *req) { r.token = viewerToken })
	if len(h.auth.logouts) != 1 || h.auth.logouts[0] != viewerToken {
		t.Fatalf("previous session not revoked: %v", h.auth.logouts)
	}
}

func TestLoginFailuresAreUniform(t *testing.T) {
	h := newHarness(t)
	loginOK(h.auth, nil)
	wrong := h.do(req{method: "POST", path: "/v1/auth/login", op: "/v1/auth/login", body: `{"email":"riya@arviawealth.com","password":"nope"}`})
	unknown := h.do(req{method: "POST", path: "/v1/auth/login", op: "/v1/auth/login", body: `{"email":"ghost@arviawealth.com","password":"nope"}`})
	locked := h.do(req{method: "POST", path: "/v1/auth/login", op: "/v1/auth/login", body: `{"email":"locked@arviawealth.com","password":"nope"}`})
	strip := func(s string) string { return s[:strings.Index(s, `"request_id"`)] }
	if wrong.Code != 401 || unknown.Code != 401 || locked.Code != 401 ||
		strip(wrong.Body.String()) != strip(unknown.Body.String()) || strip(unknown.Body.String()) != strip(locked.Body.String()) {
		t.Fatalf("responses differ:\n%s\n%s\n%s", wrong.Body, unknown.Body, locked.Body)
	}
}

func TestLoginRateLimitDoesNotRevealAccounts(t *testing.T) {
	h := newHarness(t, func(c *Config, _ *Deps) { c.LoginIPRate = Rate{Burst: 100, Per: time.Minute} })
	loginOK(h.auth, nil)
	// Existing and unknown emails reach 429 after the same number of failures.
	for _, email := range []string{"riya@arviawealth.com", "ghost@arviawealth.com"} {
		for i := range 4 {
			if res := login(h, email, "nope", fromIP(i)); res.StatusCode != 401 {
				t.Fatalf("%s attempt %d: %d", email, i, res.StatusCode)
			}
		}
		if res := login(h, email, "nope", fromIP(9)); res.StatusCode != 429 {
			t.Fatalf("%s: 5th failure should be 429, got %d", email, res.StatusCode)
		}
		// Blocked even with the right password until the window passes.
		if res := login(h, email, "right-password", fromIP(8)); res.StatusCode != 429 {
			t.Fatalf("%s: blocked email should stay 429, got %d", email, res.StatusCode)
		}
	}
	h.clock.Advance(16 * time.Minute)
	if res := login(h, "riya@arviawealth.com", "right-password", fromIP(7)); res.StatusCode != 200 {
		t.Fatalf("after window: %d", res.StatusCode)
	}
}

func TestLoginPerIPLimit(t *testing.T) {
	h := newHarness(t, func(c *Config, _ *Deps) { c.LoginIPRate = Rate{Burst: 3, Per: time.Minute} })
	loginOK(h.auth, nil)
	for i := range 3 {
		if res := login(h, "user"+string(rune('a'+i))+"@x.co", "nope"); res.StatusCode != 401 {
			t.Fatalf("attempt %d: %d", i, res.StatusCode)
		}
	}
	if res := login(h, "other@x.co", "nope"); res.StatusCode != 429 {
		t.Fatalf("per-IP limit: %d", res.StatusCode)
	}
}

// fromIP sends the request through the trusted proxy with a distinct client
// address, so only the per-email budget applies.
func fromIP(i int) func(*req) {
	return func(r *req) {
		r.peer = "10.0.0.1:1234" // the trusted load balancer
		r.headers = map[string]string{"X-Forwarded-For": "198.51.100." + strconv.Itoa(i+1)}
	}
}

func TestLoginValidation(t *testing.T) {
	h := newHarness(t)
	loginOK(h.auth, nil)
	rec := h.do(req{method: "POST", path: "/v1/auth/login", op: "/v1/auth/login", body: `{"email":"a@b.co"}`})
	wantCode(t, rec, 400, "validation_failed")
	rec = h.do(req{method: "POST", path: "/v1/auth/login", op: "/v1/auth/login", body: `{"email":"a@b.co","password":"x","remember":true}`})
	wantCode(t, rec, 400, "validation_failed")
}

func TestLogout(t *testing.T) {
	h := newHarness(t)
	rec := h.as(analystToken, req{method: "POST", path: "/v1/auth/logout", op: "/v1/auth/logout"})
	wantStatus(t, rec, 204)
	if len(h.auth.logouts) != 1 {
		t.Fatal("session not revoked")
	}
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge >= 0 {
			t.Fatalf("cookie %s not cleared", c.Name)
		}
	}
	wantCode(t, h.do(req{method: "POST", path: "/v1/auth/logout", op: "/v1/auth/logout", token: analystToken}), 401, "unauthorized")
	wantCode(t, h.do(req{method: "POST", path: "/v1/auth/logout", op: "/v1/auth/logout", bearer: adminKey}), 403, "forbidden")
}
