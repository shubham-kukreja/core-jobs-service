// Package auth authenticates users (email + password now; OAuth/SSO later as
// additional identity providers) and authorises requests by role.
//
// Security properties every implementation must keep:
//   - passwords are hashed with argon2id; plaintext never leaves Login;
//   - session tokens and API keys are 256-bit random values, stored only as
//     SHA-256 hashes and compared in constant time;
//   - unknown email and wrong password are indistinguishable (same error,
//     same timing via a dummy hash);
//   - repeated failures lock the account temporarily;
//   - sessions have idle and absolute expiry, rotate on login, and are
//     revoked on logout, password change and user disable.
package auth

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

// Role is a user's permission level. Roles are ordered: admin ⊃ analyst ⊃ viewer.
type Role string

const (
	RoleViewer  Role = "viewer"
	RoleAnalyst Role = "analyst"
	RoleAdmin   Role = "admin"
)

var roleRank = map[Role]int{RoleViewer: 1, RoleAnalyst: 2, RoleAdmin: 3}

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return roleRank[r] > 0 }

// AtLeast reports whether r grants everything min does.
func (r Role) AtLeast(min Role) bool { return r.Valid() && roleRank[r] >= roleRank[min] }

// PrincipalKind is how a request authenticated.
type PrincipalKind string

const (
	PrincipalSession PrincipalKind = "session"
	PrincipalAPIKey  PrincipalKind = "api_key"
)

// Principal is the authenticated caller of a request.
type Principal struct {
	UserID   int64
	UserUUID uuid.UUID
	Email    string
	Role     Role
	Kind     PrincipalKind
	// CSRFHash is set for session principals; cookie-authenticated unsafe
	// requests must present the matching token.
	CSRFHash []byte
}

// User is an account.
type User struct {
	ID          int64
	UUID        uuid.UUID
	Email       string
	Name        string
	Role        Role
	Status      string // active | disabled
	LastLoginAt *time.Time
	CreatedAt   time.Time
}

// ClientInfo is request metadata recorded with sessions for security
// monitoring.
type ClientInfo struct {
	IP        netip.Addr
	UserAgent string
	// RequestID ties login and logout audit rows to the request log.
	RequestID string
}

// Session is returned once at login. Token and CSRFToken are plaintext and
// must only be sent to the client (cookie + response); they are not stored.
type Session struct {
	Token     string
	CSRFToken string
	User      User
	ExpiresAt time.Time
}

// Authenticator validates credentials and sessions.
type Authenticator interface {
	// Login verifies email + password and creates a session. Failures return
	// ErrInvalidCredentials or ErrLocked only.
	Login(ctx context.Context, email, password string, ci ClientInfo) (Session, error)
	Logout(ctx context.Context, sessionToken string) error
	// Session resolves a session token, extending its idle expiry.
	Session(ctx context.Context, token string) (Principal, error)
	APIKey(ctx context.Context, key string) (Principal, error)
}

// IdentityProvider is an external login (OAuth/OIDC) added later. It
// resolves a provider callback to a stable subject; Users links the subject
// to an existing user (no self-signup).
type IdentityProvider interface {
	Name() string
	AuthCodeURL(state, nonce string) string
	Exchange(ctx context.Context, code, nonce string) (subject, email string, err error)
}

// APIKey is the metadata of a key. The secret is shown once at creation.
type APIKey struct {
	ID         uuid.UUID
	Prefix     string
	Label      string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
}

// Users is account administration (admin CLI and admin endpoints).
type Users interface {
	Create(ctx context.Context, email, name string, role Role) (User, error)
	Get(ctx context.Context, id int64) (User, error)
	SetPassword(ctx context.Context, userID int64, password string) error
	SetRole(ctx context.Context, userID int64, role Role) error
	Disable(ctx context.Context, userID int64) error
	CreateAPIKey(ctx context.Context, userID int64, label string, ttl time.Duration) (secret string, key APIKey, err error)
	RevokeAPIKey(ctx context.Context, userID int64, id uuid.UUID) error
	ListAPIKeys(ctx context.Context, userID int64) ([]APIKey, error)
}

// PasswordPolicy validates new passwords (length, breached/common list).
type PasswordPolicy interface {
	Check(password, email string) error
}

var (
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	ErrLocked             = errors.New("auth: account temporarily locked")
	ErrSessionInvalid     = errors.New("auth: session invalid or expired")
	ErrWeakPassword       = errors.New("auth: password does not meet policy")
	ErrNotFound           = errors.New("auth: not found")
)
