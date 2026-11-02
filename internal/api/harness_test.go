package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/audit"
	auditmocks "github.com/shubham-kukreja/core-jobs-service/internal/audit/mocks"
	"github.com/shubham-kukreja/core-jobs-service/internal/auth"
	catalogmocks "github.com/shubham-kukreja/core-jobs-service/internal/catalog/mocks"
	dpmocks "github.com/shubham-kukreja/core-jobs-service/internal/datapoints/mocks"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
)

// fakeTx satisfies pgx.Tx for handlers that only pass it through.
type fakeTx struct {
	pgx.Tx
	db *fakeDB
}

func (t *fakeTx) Commit(context.Context) error {
	t.db.mu.Lock()
	defer t.db.mu.Unlock()
	t.db.commits++
	return nil
}

func (t *fakeTx) Rollback(context.Context) error {
	t.db.mu.Lock()
	defer t.db.mu.Unlock()
	t.db.rollbacks++
	return nil
}

type fakeDB struct {
	mu                 sync.Mutex
	commits, rollbacks int
}

func (d *fakeDB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) { return &fakeTx{db: d}, nil }

// fakeAuth maps tokens to principals.
type fakeAuth struct {
	sessions map[string]auth.Principal
	keys     map[string]auth.Principal
	login    func(email, pw string) (auth.Session, error)
	logouts  []string
}

func (f *fakeAuth) Login(_ context.Context, email, pw string, _ auth.ClientInfo) (auth.Session, error) {
	return f.login(email, pw)
}

func (f *fakeAuth) Logout(_ context.Context, token string) error {
	f.logouts = append(f.logouts, token)
	delete(f.sessions, token)
	return nil
}

func (f *fakeAuth) Session(_ context.Context, token string) (auth.Principal, error) {
	if p, ok := f.sessions[token]; ok {
		return p, nil
	}
	return auth.Principal{}, auth.ErrSessionInvalid
}

func (f *fakeAuth) APIKey(_ context.Context, key string) (auth.Principal, error) {
	if p, ok := f.keys[key]; ok {
		return p, nil
	}
	return auth.Principal{}, auth.ErrSessionInvalid
}

type fakeUsers map[int64]auth.User

func (f fakeUsers) Get(_ context.Context, id int64) (auth.User, error) {
	if u, ok := f[id]; ok {
		return u, nil
	}
	return auth.User{}, auth.ErrNotFound
}

type memIdem struct {
	mu   sync.Mutex
	recs map[string]IdemRecord
}

func (m *memIdem) Get(_ context.Context, uid int64, key string, now time.Time) (*IdemRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.recs[key]; ok && r.UserID == uid && r.ExpiresAt.After(now) {
		return &r, nil
	}
	return nil, nil
}

func (m *memIdem) Put(_ context.Context, _ pgx.Tx, rec IdemRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.recs[rec.Key]; ok {
		return errIdemRace
	}
	m.recs[rec.Key] = rec
	return nil
}

type fakeRebuilder struct{ calls [][]string }

func (f *fakeRebuilder) EnqueueRebuild(_ context.Context, _ pgx.Tx, keys []string, _ int64) (jobs.RunRef, error) {
	f.calls = append(f.calls, keys)
	return jobs.RunRef{ID: 7, UUID: uuid.MustParse("a1c2e3f4-5b6c-4d7e-8f90-a1b2c3d4e5f6"), Status: jobs.StatusQueued}, nil
}

// Test identities. Tokens are well-formed so they resemble real ones.
var (
	adminUser   = auth.User{ID: 1, UUID: uuid.MustParse("0b8f2d4e-1c3a-4f5b-9d6e-7a8b9c0d1e2f"), Email: "admin@arviawealth.com", Name: "Admin", Role: auth.RoleAdmin, Status: "active"}
	analystUser = auth.User{ID: 2, UUID: uuid.MustParse("1c9f3e5f-2d4b-4a6c-8e7f-8b9c0d1e2f3a"), Email: "riya@arviawealth.com", Name: "Riya", Role: auth.RoleAnalyst, Status: "active"}
	viewerUser  = auth.User{ID: 3, UUID: uuid.MustParse("2daf4f60-3e5c-4b7d-9f80-9c0d1e2f3a4b"), Email: "view@arviawealth.com", Name: "Viewer", Role: auth.RoleViewer, Status: "active"}
)

const (
	adminToken   = "adminTokenAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	analystToken = "analystTokenAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	viewerToken  = "viewerTokenAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	csrfToken    = "csrfTokenAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	adminKey     = "core_deadbeef_keyAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

func principalFor(u auth.User, kind auth.PrincipalKind) auth.Principal {
	p := auth.Principal{UserID: u.ID, UserUUID: u.UUID, Email: u.Email, Role: u.Role, Kind: kind}
	if kind == auth.PrincipalSession {
		p.CSRFHash = auth.HashToken(csrfToken)
	}
	return p
}

// harness wires a Server with fakes and records audit entries.
type harness struct {
	t       *testing.T
	srv     *Server
	h       http.Handler
	db      *fakeDB
	auth    *fakeAuth
	cat     *catalogmocks.Catalog
	reader  *dpmocks.Reader
	writer  *dpmocks.Writer
	rebuild *fakeRebuilder
	clock   *clock.Fake
	logs    *bytes.Buffer
	audits  []audit.Entry
}

var testNow = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func newHarness(t *testing.T, mutate ...func(*Config, *Deps)) *harness {
	t.Helper()
	hs := &harness{
		t: t, db: &fakeDB{}, cat: &catalogmocks.Catalog{}, reader: &dpmocks.Reader{}, writer: &dpmocks.Writer{},
		rebuild: &fakeRebuilder{}, clock: clock.NewFake(testNow), logs: &bytes.Buffer{},
		auth: &fakeAuth{
			sessions: map[string]auth.Principal{
				adminToken:   principalFor(adminUser, auth.PrincipalSession),
				analystToken: principalFor(analystUser, auth.PrincipalSession),
				viewerToken:  principalFor(viewerUser, auth.PrincipalSession),
			},
			keys: map[string]auth.Principal{adminKey: principalFor(adminUser, auth.PrincipalAPIKey)},
		},
	}
	auditor := &auditmocks.Auditor{RecordFunc: func(_ context.Context, _ pgx.Tx, e audit.Entry) error {
		hs.audits = append(hs.audits, e)
		return nil
	}}
	cfg := Config{
		CORSOrigins: []string{"https://core.arviawealth.com"}, MaxRequestBytes: 1 << 16,
		SecretKey: bytes.Repeat([]byte("k"), 32), Production: true,
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	}
	deps := Deps{
		DB: hs.db, Auth: hs.auth, Users: fakeUsers{1: adminUser, 2: analystUser, 3: viewerUser}, Audit: auditor,
		Catalog: hs.cat, Reader: hs.reader, Writer: hs.writer, Rebuilds: hs.rebuild,
		Idempotency: &memIdem{recs: map[string]IdemRecord{}}, Clock: hs.clock,
		Log: slog.New(slog.NewJSONHandler(hs.logs, nil)),
	}
	for _, m := range mutate {
		m(&cfg, &deps)
	}
	s, err := newServer(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	hs.srv, hs.h = s, s.routes()
	return hs
}

// req describes one call. op is the OpenAPI path template the response is
// validated against ("" to skip, e.g. for unknown routes).
type req struct {
	method, path, op string
	body             string
	token            string // session cookie
	csrf             string // X-CSRF-Token
	bearer           string
	headers          map[string]string
	peer             string // RemoteAddr; default an untrusted client
}

func (h *harness) do(r req) *httptest.ResponseRecorder {
	h.t.Helper()
	var body io.Reader
	if r.body != "" {
		body = strings.NewReader(r.body)
	}
	hr := httptest.NewRequest(r.method, "https://core.arviawealth.com"+r.path, body)
	hr.RemoteAddr = "203.0.113.9:4444"
	if r.peer != "" {
		hr.RemoteAddr = r.peer
	}
	if r.body != "" {
		hr.Header.Set("Content-Type", "application/json")
	}
	if r.token != "" {
		hr.AddCookie(&http.Cookie{Name: sessionCookie, Value: r.token})
	}
	if r.csrf != "" {
		hr.Header.Set(csrfHeader, r.csrf)
	}
	if r.bearer != "" {
		hr.Header.Set("Authorization", "Bearer "+r.bearer)
	}
	for k, v := range r.headers {
		hr.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, hr)
	if r.op != "" {
		conform(h.t, r.method, r.op, rec)
	}
	return rec
}

// admin, analyst, viewer issue cookie-authenticated requests with a valid CSRF token.
func (h *harness) as(token string, r req) *httptest.ResponseRecorder {
	h.t.Helper()
	r.token, r.csrf = token, csrfToken
	return h.do(r)
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status %d, want %d; body: %s", rec.Code, want, rec.Body.String())
	}
}

func wantCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	wantStatus(t, rec, status)
	if !strings.Contains(rec.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("want code %s, body: %s", code, rec.Body.String())
	}
}
