package auth

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/shubham-kukreja/core-jobs-service/internal/audit"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
)

// Config tunes sessions and lockout.
type Config struct {
	SessionIdleTTL     time.Duration
	SessionAbsoluteTTL time.Duration
	// MaxFailures consecutive failed logins lock the account for LockBase,
	// doubling with each further failure up to LockMax.
	MaxFailures int
	LockBase    time.Duration
	LockMax     time.Duration
	// Session and API-key last-seen writes happen at most this often.
	TouchEvery time.Duration
}

// DefaultConfig returns secure defaults; TTLs normally come from platform config.
func DefaultConfig() Config {
	return Config{
		SessionIdleTTL:     30 * time.Minute,
		SessionAbsoluteTTL: 12 * time.Hour,
		MaxFailures:        5,
		LockBase:           15 * time.Minute,
		LockMax:            24 * time.Hour,
		TouchEvery:         time.Minute,
	}
}

const maxUserAgent = 512

// Service implements Authenticator and Users.
type Service struct {
	st     store
	hasher passwordHasher
	policy PasswordPolicy
	clock  clock.Clock
	cfg    Config
}

var (
	_ Authenticator = (*Service)(nil)
	_ Users         = (*Service)(nil)
)

// NewService wires the service. Use NewPGStore for st in production.
func NewService(st store, h passwordHasher, p PasswordPolicy, c clock.Clock, cfg Config) (*Service, error) {
	if cfg.SessionIdleTTL <= 0 || cfg.SessionAbsoluteTTL < cfg.SessionIdleTTL || cfg.MaxFailures < 1 ||
		cfg.LockBase <= 0 || cfg.LockMax < cfg.LockBase || cfg.TouchEvery <= 0 {
		return nil, errors.New("auth: invalid config")
	}
	return &Service{st: st, hasher: h, policy: p, clock: c, cfg: cfg}, nil
}

// ---------------------------------------------------------------- Authenticator

// Login verifies credentials and creates a session. Every path performs
// exactly one argon2 verification, so timing does not reveal whether the
// email exists, is locked or has no password.
func (s *Service) Login(ctx context.Context, email, password string, ci ClientInfo) (Session, error) {
	now := s.clock.Now()
	email = normalizeEmail(email)
	if utf8.RuneCountInString(password) > MaxPasswordLen*4 || email == "" {
		// Absurd inputs: still spend the verification so timing stays flat.
		_, _ = s.hasher.Verify(ctx, password, s.hasher.Dummy())
		return Session{}, ErrInvalidCredentials
	}

	lu, err := s.st.UserForLogin(ctx, email)
	switch {
	case errors.Is(err, ErrNotFound):
		_, _ = s.hasher.Verify(ctx, password, s.hasher.Dummy())
		// No email for unknown users: the audit row must not collect
		// addresses people typed by mistake.
		_ = s.st.RecordAudit(ctx, loginEntry(nil, ci, "unknown", "login_failed"))
		return Session{}, ErrInvalidCredentials
	case err != nil:
		return Session{}, err
	}
	uid := lu.User.ID
	entityID := lu.User.UUID.String()

	if lu.LockedUntil != nil && now.Before(*lu.LockedUntil) {
		_, _ = s.hasher.Verify(ctx, password, s.hasher.Dummy())
		_ = s.st.RecordAudit(ctx, loginEntry(&uid, ci, entityID, "login_locked"))
		return Session{}, ErrLocked
	}

	phc := lu.PasswordHash
	if phc == "" {
		phc = s.hasher.Dummy()
	}
	ok, verr := s.hasher.Verify(ctx, password, phc)
	if verr != nil && !errors.Is(verr, context.Canceled) && !errors.Is(verr, context.DeadlineExceeded) {
		// A malformed stored hash: fail closed, never log the hash.
		verr = fmt.Errorf("auth: stored hash for user %d unusable", uid)
	}
	if verr != nil {
		return Session{}, verr
	}
	if !ok || lu.PasswordHash == "" || lu.User.Status != "active" {
		failed := lu.FailedLogins + 1
		var lockedUntil *time.Time
		if failed >= s.cfg.MaxFailures {
			t := now.Add(s.lockDuration(failed))
			lockedUntil = &t
		}
		if err := s.st.LoginFailed(ctx, uid, failed, lockedUntil, now, loginEntry(&uid, ci, entityID, "login_failed")); err != nil {
			return Session{}, err
		}
		return Session{}, ErrInvalidCredentials
	}

	token, err := newToken()
	if err != nil {
		return Session{}, err
	}
	csrf, err := newToken()
	if err != nil {
		return Session{}, err
	}
	expires := now.Add(s.cfg.SessionAbsoluteTTL)
	ns := newSession{
		TokenHash: HashToken(token), CSRFHash: HashToken(csrf), UserID: uid, Now: now,
		IdleExpiresAt: minTime(now.Add(s.cfg.SessionIdleTTL), expires), ExpiresAt: expires,
		IP: ci.IP, UserAgent: truncate(ci.UserAgent, maxUserAgent),
	}
	if err := s.st.LoginSucceeded(ctx, ns, loginEntry(&uid, ci, entityID, "login")); err != nil {
		return Session{}, err
	}
	u := lu.User
	u.LastLoginAt = &now
	return Session{Token: token, CSRFToken: csrf, User: u, ExpiresAt: expires}, nil
}

func (s *Service) lockDuration(failed int) time.Duration {
	d := s.cfg.LockBase
	for i := s.cfg.MaxFailures; i < failed && d < s.cfg.LockMax; i++ {
		d *= 2
	}
	return min(d, s.cfg.LockMax)
}

// Logout revokes the session. Unknown or already revoked tokens are not an
// error: logout is idempotent.
func (s *Service) Logout(ctx context.Context, token string) error {
	if !validToken(token) {
		return nil
	}
	h := HashToken(token)
	su, err := s.st.SessionByHash(ctx, h)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	uid := su.User.ID
	err = s.st.RevokeSession(ctx, h, s.clock.Now(), audit.Entry{
		ActorID: &uid, ActorKind: audit.ActorUser, Entity: "session", EntityID: su.User.UUID.String(), Action: "logout",
	})
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// Session resolves a token to its principal and slides the idle expiry.
func (s *Service) Session(ctx context.Context, token string) (Principal, error) {
	if !validToken(token) {
		return Principal{}, ErrSessionInvalid
	}
	h := HashToken(token)
	su, err := s.st.SessionByHash(ctx, h)
	if errors.Is(err, ErrNotFound) {
		return Principal{}, ErrSessionInvalid
	}
	if err != nil {
		return Principal{}, err
	}
	now := s.clock.Now()
	if su.RevokedAt != nil || !now.Before(su.ExpiresAt) || !now.Before(su.IdleExpiresAt) || su.User.Status != "active" {
		return Principal{}, ErrSessionInvalid
	}
	if now.Sub(su.LastSeenAt) >= s.cfg.TouchEvery {
		idle := minTime(now.Add(s.cfg.SessionIdleTTL), su.ExpiresAt)
		if err := s.st.TouchSession(ctx, h, now, idle); err != nil {
			return Principal{}, err
		}
	}
	return Principal{
		UserID: su.User.ID, UserUUID: su.User.UUID, Email: su.User.Email, Role: su.User.Role,
		Kind: PrincipalSession, CSRFHash: su.CSRFHash,
	}, nil
}

// APIKey resolves an API key to its principal.
func (s *Service) APIKey(ctx context.Context, key string) (Principal, error) {
	if !apiKeyRE.MatchString(key) {
		return Principal{}, ErrSessionInvalid
	}
	h := HashToken(key)
	k, err := s.st.APIKeyByHash(ctx, h)
	if errors.Is(err, ErrNotFound) {
		return Principal{}, ErrSessionInvalid
	}
	if err != nil {
		return Principal{}, err
	}
	now := s.clock.Now()
	if !TokenMatches(key, k.Hash) || k.RevokedAt != nil || (k.ExpiresAt != nil && !now.Before(*k.ExpiresAt)) ||
		k.User.Status != "active" {
		return Principal{}, ErrSessionInvalid
	}
	if k.LastUsedAt == nil || now.Sub(*k.LastUsedAt) >= 5*s.cfg.TouchEvery {
		if err := s.st.TouchAPIKey(ctx, k.ID, now); err != nil {
			return Principal{}, err
		}
	}
	return Principal{
		UserID: k.User.ID, UserUUID: k.User.UUID, Email: k.User.Email, Role: k.User.Role, Kind: PrincipalAPIKey,
	}, nil
}

// ---------------------------------------------------------------- Users

// Create adds a user. There is no self-signup: only admins call this.
func (s *Service) Create(ctx context.Context, email, name string, role Role) (User, error) {
	email = normalizeEmail(email)
	name = strings.TrimSpace(name)
	var details []apperr.Detail
	if email == "" {
		details = append(details, apperr.Detail{Field: "email", Reason: "invalid_email"})
	}
	if n := utf8.RuneCountInString(name); n < 1 || n > 200 {
		details = append(details, apperr.Detail{Field: "name", Reason: "invalid_length"})
	}
	if !role.Valid() {
		details = append(details, apperr.Detail{Field: "role", Reason: "unknown_role"})
	}
	if len(details) > 0 {
		return User{}, apperr.New(apperr.ValidationFailed, "invalid user", details...)
	}
	return s.st.CreateUser(ctx, email, name, role, systemEntry("user", "", "create", map[string]any{"role": role}))
}

// Get returns a user by id.
func (s *Service) Get(ctx context.Context, id int64) (User, error) { return s.st.UserByID(ctx, id) }

// ByEmail returns a user by email (admin CLI).
func (s *Service) ByEmail(ctx context.Context, email string) (User, error) {
	email = normalizeEmail(email)
	if email == "" {
		return User{}, ErrNotFound
	}
	return s.st.UserByEmail(ctx, email)
}

// SetPassword checks the policy, stores a new hash and revokes every session.
func (s *Service) SetPassword(ctx context.Context, userID int64, password string) error {
	u, err := s.st.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if err := s.policy.Check(password, u.Email); err != nil {
		return err
	}
	phc, err := s.hasher.Hash(ctx, password)
	if err != nil {
		return err
	}
	return s.st.SetPassword(ctx, userID, u.Email, phc, s.clock.Now(), systemEntry("user", u.UUID.String(), "set_password", nil))
}

// SetRole changes a user's role and revokes their sessions.
func (s *Service) SetRole(ctx context.Context, userID int64, role Role) error {
	if !role.Valid() {
		return apperr.New(apperr.ValidationFailed, "unknown role", apperr.Detail{Field: "role", Reason: "unknown_role"})
	}
	u, err := s.st.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	e := systemEntry("user", u.UUID.String(), "set_role", map[string]any{"role": role})
	e.Before = map[string]any{"role": u.Role}
	return s.st.SetRole(ctx, userID, role, s.clock.Now(), e)
}

// Disable blocks a user and revokes their sessions. API keys stop working
// because APIKey checks the user's status.
func (s *Service) Disable(ctx context.Context, userID int64) error {
	u, err := s.st.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	return s.st.SetStatus(ctx, userID, "disabled", s.clock.Now(), systemEntry("user", u.UUID.String(), "disable", nil))
}

// CreateAPIKey returns the key secret once; only its hash is stored.
func (s *Service) CreateAPIKey(ctx context.Context, userID int64, label string, ttl time.Duration) (string, APIKey, error) {
	if utf8.RuneCountInString(label) > 100 {
		return "", APIKey{}, apperr.New(apperr.ValidationFailed, "label too long", apperr.Detail{Field: "label", Reason: "invalid_length"})
	}
	if ttl < 0 {
		return "", APIKey{}, apperr.New(apperr.ValidationFailed, "ttl must not be negative", apperr.Detail{Field: "ttl", Reason: "out_of_range"})
	}
	u, err := s.st.UserByID(ctx, userID)
	if err != nil {
		return "", APIKey{}, err
	}
	secret, prefix, err := newAPIKey()
	if err != nil {
		return "", APIKey{}, err
	}
	now := s.clock.Now()
	row := newAPIKeyRow{UserID: userID, Prefix: prefix, Hash: HashToken(secret), Label: label, Now: now}
	if ttl > 0 {
		exp := now.Add(ttl)
		row.ExpiresAt = &exp
	}
	k, err := s.st.InsertAPIKey(ctx, row, systemEntry("api_key", u.UUID.String(), "create", map[string]any{"prefix": prefix, "label": label}))
	if err != nil {
		return "", APIKey{}, err
	}
	return secret, k, nil
}

// RevokeAPIKey revokes one of the user's keys.
func (s *Service) RevokeAPIKey(ctx context.Context, userID int64, id uuid.UUID) error {
	return s.st.RevokeAPIKey(ctx, userID, id, s.clock.Now(), systemEntry("api_key", id.String(), "revoke", nil))
}

// ListAPIKeys lists key metadata (never secrets).
func (s *Service) ListAPIKeys(ctx context.Context, userID int64) ([]APIKey, error) {
	return s.st.ListAPIKeys(ctx, userID)
}

// ---------------------------------------------------------------- helpers

// normalizeEmail returns the lowercase address, or "" if it is not a plain
// address (no display name, no comments).
func normalizeEmail(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) < 3 || len(s) > 254 {
		return ""
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || a.Name != "" {
		return ""
	}
	return s
}

func loginEntry(uid *int64, ci ClientInfo, entityID, action string) audit.Entry {
	kind := audit.ActorUser
	if uid == nil {
		kind = audit.ActorSystem
	}
	return audit.Entry{ActorID: uid, ActorKind: kind, RequestID: ci.RequestID, IP: ci.IP, Entity: "session", EntityID: entityID, Action: action}
}

func systemEntry(entity, id, action string, after any) audit.Entry {
	return audit.Entry{ActorKind: audit.ActorSystem, Entity: entity, EntityID: id, Action: action, After: after}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
