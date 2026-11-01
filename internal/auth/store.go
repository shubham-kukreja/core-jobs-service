package auth

import (
	"context"
	"net/netip"
	"time"

	"github.com/google/uuid"

	"github.com/shubham-kukreja/core-jobs-service/internal/audit"
)

// loginUser is what Login needs about an account.
type loginUser struct {
	User         User
	PasswordHash string // empty when the user has no password credential
	FailedLogins int
	LockedUntil  *time.Time
}

// newSession is a session row to insert.
type newSession struct {
	TokenHash     []byte
	CSRFHash      []byte
	UserID        int64
	Now           time.Time
	IdleExpiresAt time.Time
	ExpiresAt     time.Time
	IP            netip.Addr
	UserAgent     string
}

// sessionUser is a stored session with its user.
type sessionUser struct {
	CSRFHash      []byte
	LastSeenAt    time.Time
	IdleExpiresAt time.Time
	ExpiresAt     time.Time
	RevokedAt     *time.Time
	User          User
}

// apiKeyUser is a stored API key with its user.
type apiKeyUser struct {
	ID         int64
	Hash       []byte
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
	User       User
}

// newAPIKeyRow is an API key to insert.
type newAPIKeyRow struct {
	UserID    int64
	Prefix    string
	Hash      []byte
	Label     string
	Now       time.Time
	ExpiresAt *time.Time
}

// store persists accounts. Every method that changes state also writes its
// audit entry in the same transaction, so a change is never unaudited.
type store interface {
	UserForLogin(ctx context.Context, email string) (loginUser, error)
	UserByID(ctx context.Context, id int64) (User, error)
	UserByEmail(ctx context.Context, email string) (User, error)

	LoginFailed(ctx context.Context, userID int64, failed int, lockedUntil *time.Time, now time.Time, e audit.Entry) error
	RecordAudit(ctx context.Context, e audit.Entry) error
	LoginSucceeded(ctx context.Context, s newSession, e audit.Entry) error

	SessionByHash(ctx context.Context, hash []byte) (sessionUser, error)
	TouchSession(ctx context.Context, hash []byte, now, idleExpiresAt time.Time) error
	// RevokeSession returns ErrNotFound when no active session matches.
	RevokeSession(ctx context.Context, hash []byte, now time.Time, e audit.Entry) error

	APIKeyByHash(ctx context.Context, hash []byte) (apiKeyUser, error)
	TouchAPIKey(ctx context.Context, id int64, now time.Time) error

	// CreateUser sets e.EntityID to the new user's UUID.
	CreateUser(ctx context.Context, email, name string, role Role, e audit.Entry) (User, error)
	// SetPassword also links the password identity and revokes all sessions.
	SetPassword(ctx context.Context, userID int64, email, hash string, now time.Time, e audit.Entry) error
	// SetRole and SetStatus revoke all sessions.
	SetRole(ctx context.Context, userID int64, role Role, now time.Time, e audit.Entry) error
	SetStatus(ctx context.Context, userID int64, status string, now time.Time, e audit.Entry) error

	InsertAPIKey(ctx context.Context, k newAPIKeyRow, e audit.Entry) (APIKey, error)
	RevokeAPIKey(ctx context.Context, userID int64, id uuid.UUID, now time.Time, e audit.Entry) error
	ListAPIKeys(ctx context.Context, userID int64) ([]APIKey, error)
}

// passwordHasher is satisfied by *Hasher; tests substitute a fake.
type passwordHasher interface {
	Hash(ctx context.Context, password string) (string, error)
	Verify(ctx context.Context, password, phc string) (bool, error)
	Dummy() string
}
