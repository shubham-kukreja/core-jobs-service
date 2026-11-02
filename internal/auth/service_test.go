package auth

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/shubham-kukreja/core-jobs-service/internal/audit"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
)

// fakeHasher records which hash each Verify ran against.
type fakeHasher struct {
	mu       sync.Mutex
	verified []string
}

const fakeDummy = "$argon2id$dummy"

func (f *fakeHasher) Hash(_ context.Context, pw string) (string, error) {
	return "$argon2id$" + pw, nil
}
func (f *fakeHasher) Dummy() string { return fakeDummy }
func (f *fakeHasher) Verify(_ context.Context, pw, phc string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verified = append(f.verified, phc)
	return phc == "$argon2id$"+pw, nil
}

// memStore is an in-memory store.
type memStore struct {
	users    map[string]*loginUser // by email
	sessions map[string]*sessionUser
	keys     map[string]*apiKeyUser
	audits   []audit.Entry
	touches  int
	revoked  []int64 // user ids whose sessions were all revoked
}

func newMemStore() *memStore {
	return &memStore{users: map[string]*loginUser{}, sessions: map[string]*sessionUser{}, keys: map[string]*apiKeyUser{}}
}

func (m *memStore) add(email, pw, status string) *loginUser {
	lu := &loginUser{User: User{ID: int64(len(m.users) + 1), UUID: uuid.New(), Email: email, Name: "N", Role: RoleAnalyst, Status: status}}
	if pw != "" {
		lu.PasswordHash = "$argon2id$" + pw
	}
	m.users[email] = lu
	return lu
}

func (m *memStore) byID(id int64) *loginUser {
	for _, u := range m.users {
		if u.User.ID == id {
			return u
		}
	}
	return nil
}

func (m *memStore) UserForLogin(_ context.Context, email string) (loginUser, error) {
	if u, ok := m.users[email]; ok {
		return *u, nil
	}
	return loginUser{}, ErrNotFound
}
func (m *memStore) UserByID(_ context.Context, id int64) (User, error) {
	if u := m.byID(id); u != nil {
		return u.User, nil
	}
	return User{}, ErrNotFound
}
func (m *memStore) UserByEmail(_ context.Context, email string) (User, error) {
	if u, ok := m.users[email]; ok {
		return u.User, nil
	}
	return User{}, ErrNotFound
}
func (m *memStore) LoginFailed(_ context.Context, id int64, failed int, lu *time.Time, _ time.Time, e audit.Entry) error {
	u := m.byID(id)
	u.FailedLogins, u.LockedUntil = failed, lu
	m.audits = append(m.audits, e)
	return nil
}
func (m *memStore) RecordAudit(_ context.Context, e audit.Entry) error {
	m.audits = append(m.audits, e)
	return nil
}
func (m *memStore) LoginSucceeded(_ context.Context, s newSession, e audit.Entry) error {
	u := m.byID(s.UserID)
	u.FailedLogins, u.LockedUntil = 0, nil
	m.sessions[string(s.TokenHash)] = &sessionUser{CSRFHash: s.CSRFHash, LastSeenAt: s.Now, IdleExpiresAt: s.IdleExpiresAt, ExpiresAt: s.ExpiresAt, User: u.User}
	m.audits = append(m.audits, e)
	return nil
}
func (m *memStore) SessionByHash(_ context.Context, h []byte) (sessionUser, error) {
	if s, ok := m.sessions[string(h)]; ok {
		s.User = m.byID(s.User.ID).User
		return *s, nil
	}
	return sessionUser{}, ErrNotFound
}
func (m *memStore) TouchSession(_ context.Context, h []byte, now, idle time.Time) error {
	m.touches++
	s := m.sessions[string(h)]
	s.LastSeenAt, s.IdleExpiresAt = now, idle
	return nil
}
func (m *memStore) RevokeSession(_ context.Context, h []byte, now time.Time, e audit.Entry) error {
	s, ok := m.sessions[string(h)]
	if !ok || s.RevokedAt != nil {
		return ErrNotFound
	}
	s.RevokedAt = &now
	m.audits = append(m.audits, e)
	return nil
}
func (m *memStore) APIKeyByHash(_ context.Context, h []byte) (apiKeyUser, error) {
	if k, ok := m.keys[string(h)]; ok {
		k.User = m.byID(k.User.ID).User
		return *k, nil
	}
	return apiKeyUser{}, ErrNotFound
}
func (m *memStore) TouchAPIKey(_ context.Context, id int64, now time.Time) error {
	for _, k := range m.keys {
		if k.ID == id {
			k.LastUsedAt = &now
		}
	}
	return nil
}
func (m *memStore) CreateUser(_ context.Context, email, name string, role Role, e audit.Entry) (User, error) {
	u := m.add(email, "", "active")
	u.User.Name, u.User.Role = name, role
	e.EntityID = u.User.UUID.String()
	m.audits = append(m.audits, e)
	return u.User, nil
}
func (m *memStore) revokeAll(id int64, now time.Time) {
	for _, s := range m.sessions {
		if s.User.ID == id && s.RevokedAt == nil {
			s.RevokedAt = &now
		}
	}
	m.revoked = append(m.revoked, id)
}
func (m *memStore) SetPassword(_ context.Context, id int64, _, hash string, now time.Time, e audit.Entry) error {
	m.byID(id).PasswordHash = hash
	m.revokeAll(id, now)
	m.audits = append(m.audits, e)
	return nil
}
func (m *memStore) SetRole(_ context.Context, id int64, r Role, now time.Time, e audit.Entry) error {
	m.byID(id).User.Role = r
	m.revokeAll(id, now)
	m.audits = append(m.audits, e)
	return nil
}
func (m *memStore) SetStatus(_ context.Context, id int64, st string, now time.Time, e audit.Entry) error {
	m.byID(id).User.Status = st
	m.revokeAll(id, now)
	m.audits = append(m.audits, e)
	return nil
}
func (m *memStore) InsertAPIKey(_ context.Context, k newAPIKeyRow, e audit.Entry) (APIKey, error) {
	m.keys[string(k.Hash)] = &apiKeyUser{ID: int64(len(m.keys) + 1), Hash: k.Hash, ExpiresAt: k.ExpiresAt, User: m.byID(k.UserID).User}
	m.audits = append(m.audits, e)
	return APIKey{ID: uuid.New(), Prefix: k.Prefix, Label: k.Label, CreatedAt: k.Now, ExpiresAt: k.ExpiresAt}, nil
}
func (m *memStore) RevokeAPIKey(context.Context, int64, uuid.UUID, time.Time, audit.Entry) error {
	return nil
}
func (m *memStore) ListAPIKeys(context.Context, int64) ([]APIKey, error) { return nil, nil }

var t0 = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

func newTestService(t *testing.T) (*Service, *memStore, *fakeHasher, *clock.Fake) {
	t.Helper()
	st, h, c := newMemStore(), &fakeHasher{}, clock.NewFake(t0)
	s, err := NewService(st, h, NewPolicy(), c, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return s, st, h, c
}

var ci = ClientInfo{IP: netip.MustParseAddr("203.0.113.7"), UserAgent: "test"}

const goodPW = "tidal-saffron-lantern-41"

func TestLoginSuccess(t *testing.T) {
	s, st, _, _ := newTestService(t)
	st.add("riya@arviawealth.com", goodPW, "active")
	sess, err := s.Login(context.Background(), "  Riya@ArviaWealth.com ", goodPW, ci)
	if err != nil {
		t.Fatal(err)
	}
	if !validToken(sess.Token) || !validToken(sess.CSRFToken) || sess.Token == sess.CSRFToken {
		t.Fatal("bad tokens")
	}
	if !sess.ExpiresAt.Equal(t0.Add(12 * time.Hour)) {
		t.Fatalf("expires %v", sess.ExpiresAt)
	}
	stored := st.sessions[string(HashToken(sess.Token))]
	if stored == nil || !TokenMatches(sess.CSRFToken, stored.CSRFHash) {
		t.Fatal("session not stored by hash")
	}
	if last := st.audits[len(st.audits)-1]; last.Action != "login" || last.IP != ci.IP {
		t.Fatalf("audit: %+v", last)
	}
}

func TestUnknownEmailVerifiesDummyAndAuditsNoEmail(t *testing.T) {
	s, st, h, _ := newTestService(t)
	_, err := s.Login(context.Background(), "nobody@arviawealth.com", goodPW, ci)
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("got %v", err)
	}
	if len(h.verified) != 1 || h.verified[0] != fakeDummy {
		t.Fatalf("want one dummy verification, got %v", h.verified)
	}
	e := st.audits[0]
	if e.ActorID != nil || strings.Contains(e.EntityID, "nobody") || e.Action != "login_failed" {
		t.Fatalf("audit leaks or wrong: %+v", e)
	}
}

func TestEveryFailurePathVerifiesOnce(t *testing.T) {
	cases := map[string]func(*memStore){
		"wrong password": func(m *memStore) { m.add("a@b.co", goodPW, "active") },
		"no password":    func(m *memStore) { m.add("a@b.co", "", "active") },
		"disabled":       func(m *memStore) { m.add("a@b.co", goodPW, "disabled") },
		"locked": func(m *memStore) {
			u := m.add("a@b.co", goodPW, "active")
			lu := t0.Add(time.Hour)
			u.LockedUntil = &lu
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			s, st, h, _ := newTestService(t)
			setup(st)
			pw := "wrong-password-123"
			if name != "wrong password" {
				pw = goodPW
			}
			_, err := s.Login(context.Background(), "a@b.co", pw, ci)
			if err == nil {
				t.Fatal("login should fail")
			}
			if len(h.verified) != 1 {
				t.Fatalf("want exactly one verification, got %d", len(h.verified))
			}
		})
	}
}

func TestLockoutAndBackoff(t *testing.T) {
	s, st, _, c := newTestService(t)
	u := st.add("a@b.co", goodPW, "active")
	ctx := context.Background()
	for i := range 5 {
		if _, err := s.Login(ctx, "a@b.co", "wrong-password-123", ci); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if u.LockedUntil == nil || !u.LockedUntil.Equal(t0.Add(15*time.Minute)) {
		t.Fatalf("locked until %v", u.LockedUntil)
	}
	if _, err := s.Login(ctx, "a@b.co", goodPW, ci); !errors.Is(err, ErrLocked) {
		t.Fatalf("correct password while locked: %v", err)
	}
	c.Advance(16 * time.Minute)
	if _, err := s.Login(ctx, "a@b.co", "wrong-password-123", ci); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal(err)
	}
	if !u.LockedUntil.Equal(c.Now().Add(30 * time.Minute)) {
		t.Fatalf("backoff should double: %v", u.LockedUntil.Sub(c.Now()))
	}
	c.Advance(31 * time.Minute)
	if _, err := s.Login(ctx, "a@b.co", goodPW, ci); err != nil {
		t.Fatalf("login after lock expiry: %v", err)
	}
	if u.FailedLogins != 0 || u.LockedUntil != nil {
		t.Fatal("success must reset failures")
	}
	if got := s.lockDuration(100); got != 24*time.Hour {
		t.Fatalf("lock cap: %v", got)
	}
}

func TestSessionExpiry(t *testing.T) {
	s, st, _, c := newTestService(t)
	st.add("a@b.co", goodPW, "active")
	ctx := context.Background()
	sess, _ := s.Login(ctx, "a@b.co", goodPW, ci)

	p, err := s.Session(ctx, sess.Token)
	if err != nil || p.Kind != PrincipalSession || p.Role != RoleAnalyst || len(p.CSRFHash) != 32 {
		t.Fatalf("session: %+v %v", p, err)
	}
	// Activity every 20 minutes keeps the session alive (idle TTL 30m)…
	for range 6 {
		c.Advance(20 * time.Minute)
		if _, err := s.Session(ctx, sess.Token); err != nil {
			t.Fatalf("at %v: %v", c.Now().Sub(t0), err)
		}
	}
	if st.touches == 0 {
		t.Fatal("idle expiry never extended")
	}
	// …but never past the absolute TTL (12h).
	c.Set(t0.Add(12 * time.Hour))
	if _, err := s.Session(ctx, sess.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("past absolute expiry: %v", err)
	}
}

func TestSessionIdleTimeoutAndRevocation(t *testing.T) {
	s, st, _, c := newTestService(t)
	u := st.add("a@b.co", goodPW, "active")
	ctx := context.Background()
	sess, _ := s.Login(ctx, "a@b.co", goodPW, ci)
	c.Advance(31 * time.Minute)
	if _, err := s.Session(ctx, sess.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("idle: %v", err)
	}

	sess2, _ := s.Login(ctx, "a@b.co", goodPW, ci)
	if err := s.SetPassword(ctx, u.User.ID, "another-strong-passphrase"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, sess2.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("password change must revoke: %v", err)
	}

	sess3, _ := s.Login(ctx, "a@b.co", "another-strong-passphrase", ci)
	if err := s.Logout(ctx, sess3.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, sess3.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("logout must revoke: %v", err)
	}
	if err := s.Logout(ctx, sess3.Token); err != nil {
		t.Fatalf("logout is idempotent: %v", err)
	}
	for _, bad := range []string{"", "x", strings.Repeat("a", 44)} {
		if _, err := s.Session(ctx, bad); !errors.Is(err, ErrSessionInvalid) {
			t.Fatalf("malformed %q: %v", bad, err)
		}
	}
}

func TestDisableRevokesAndBlocks(t *testing.T) {
	s, st, _, _ := newTestService(t)
	u := st.add("a@b.co", goodPW, "active")
	ctx := context.Background()
	sess, _ := s.Login(ctx, "a@b.co", goodPW, ci)
	key, _, err := s.CreateAPIKey(ctx, u.User.ID, "ci", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.APIKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := s.Disable(ctx, u.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, sess.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatal("disabled user's session still valid")
	}
	if _, err := s.APIKey(ctx, key); !errors.Is(err, ErrSessionInvalid) {
		t.Fatal("disabled user's API key still valid")
	}
}

func TestAPIKeys(t *testing.T) {
	s, st, _, c := newTestService(t)
	u := st.add("a@b.co", goodPW, "active")
	ctx := context.Background()
	key, meta, err := s.CreateAPIKey(ctx, u.User.ID, "ci", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "core_"+meta.Prefix+"_") {
		t.Fatalf("key %q prefix %q", key, meta.Prefix)
	}
	for _, e := range st.audits {
		if strings.Contains(e.EntityID+e.Action, key) {
			t.Fatal("secret in audit")
		}
	}
	p, err := s.APIKey(ctx, key)
	if err != nil || p.Kind != PrincipalAPIKey || len(p.CSRFHash) != 0 {
		t.Fatalf("%+v %v", p, err)
	}
	// Change the last character to a different one (it may already be 'A').
	last := "A"
	if strings.HasSuffix(key, "A") {
		last = "B"
	}
	if _, err := s.APIKey(ctx, key[:len(key)-1]+last); !errors.Is(err, ErrSessionInvalid) {
		t.Fatal("tampered key accepted")
	}
	c.Advance(time.Hour)
	if _, err := s.APIKey(ctx, key); !errors.Is(err, ErrSessionInvalid) {
		t.Fatal("expired key accepted")
	}
}

func TestCreateAndPolicy(t *testing.T) {
	s, st, _, _ := newTestService(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, "not an email", "X", RoleAdmin); err == nil {
		t.Fatal("bad email accepted")
	}
	if _, err := s.Create(ctx, "a@b.co", "X", Role("root")); err == nil {
		t.Fatal("bad role accepted")
	}
	u, err := s.Create(ctx, "New@B.co", "New User", RoleViewer)
	if err != nil || u.Email != "new@b.co" {
		t.Fatalf("%+v %v", u, err)
	}
	if err := s.SetPassword(ctx, u.ID, "password1234"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak password: %v", err)
	}
	if st.users["new@b.co"].PasswordHash != "" {
		t.Fatal("weak password stored")
	}
}
