package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shubham-kukreja/core-jobs-service/internal/audit"
	"github.com/shubham-kukreja/core-jobs-service/internal/auth/internal/db"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
)

// PGStore is the Postgres store. Pass it to NewService.
type PGStore struct {
	pool    *pgxpool.Pool
	q       *db.Queries
	auditor audit.Auditor
}

var _ store = (*PGStore)(nil)

// NewPGStore returns a store over pool that audits through auditor.
func NewPGStore(pool *pgxpool.Pool, auditor audit.Auditor) *PGStore {
	return &PGStore{pool: pool, q: db.New(pool), auditor: auditor}
}

// tx runs fn with queries bound to one transaction plus its audit entry.
func (s *PGStore) tx(ctx context.Context, e audit.Entry, fn func(q *db.Queries) error) error {
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := fn(s.q.WithTx(tx)); err != nil {
			return err
		}
		return s.auditor.Record(ctx, tx, e)
	})
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func toUser(id int64, u uuid.UUID, email, name, role, status string, lastLogin *time.Time, created time.Time) User {
	return User{ID: id, UUID: u, Email: email, Name: name, Role: Role(role), Status: status, LastLoginAt: lastLogin, CreatedAt: created}
}

// UserForLogin reads the account and its password hash.
func (s *PGStore) UserForLogin(ctx context.Context, email string) (loginUser, error) {
	r, err := s.q.UserForLogin(ctx, email)
	if err != nil {
		return loginUser{}, notFound(err)
	}
	lu := loginUser{
		User:         toUser(r.ID, r.Uuid, r.Email, r.Name, r.Role, r.Status, r.LastLoginAt, r.CreatedAt),
		FailedLogins: int(r.FailedLogins),
		LockedUntil:  r.LockedUntil,
	}
	if r.PasswordHash != nil {
		lu.PasswordHash = *r.PasswordHash
	}
	return lu, nil
}

// UserByID reads a user.
func (s *PGStore) UserByID(ctx context.Context, id int64) (User, error) {
	r, err := s.q.GetUser(ctx, id)
	if err != nil {
		return User{}, notFound(err)
	}
	return toUser(r.ID, r.Uuid, r.Email, r.Name, r.Role, r.Status, r.LastLoginAt, r.CreatedAt), nil
}

// UserByEmail reads a user.
func (s *PGStore) UserByEmail(ctx context.Context, email string) (User, error) {
	r, err := s.q.GetUserByEmail(ctx, email)
	if err != nil {
		return User{}, notFound(err)
	}
	return toUser(r.ID, r.Uuid, r.Email, r.Name, r.Role, r.Status, r.LastLoginAt, r.CreatedAt), nil
}

// LoginFailed records a failed attempt and any lock.
func (s *PGStore) LoginFailed(ctx context.Context, userID int64, failed int, lockedUntil *time.Time, now time.Time, e audit.Entry) error {
	return s.tx(ctx, e, func(q *db.Queries) error {
		return q.SetLoginFailure(ctx, db.SetLoginFailureParams{
			ID: userID, FailedLogins: int32(min(failed, 1<<30)), LockedUntil: lockedUntil, Now: now, //nolint:gosec // clamped.
		})
	})
}

// RecordAudit writes an entry on its own (failed logins for unknown emails).
func (s *PGStore) RecordAudit(ctx context.Context, e audit.Entry) error {
	return s.tx(ctx, e, func(*db.Queries) error { return nil })
}

// LoginSucceeded resets failures and inserts the session.
func (s *PGStore) LoginSucceeded(ctx context.Context, ns newSession, e audit.Entry) error {
	return s.tx(ctx, e, func(q *db.Queries) error {
		if err := q.SetLoginSuccess(ctx, db.SetLoginSuccessParams{ID: ns.UserID, Now: &ns.Now}); err != nil {
			return err
		}
		p := db.InsertSessionParams{
			TokenHash: ns.TokenHash, UserID: ns.UserID, CsrfHash: ns.CSRFHash, Now: ns.Now,
			IdleExpiresAt: ns.IdleExpiresAt, ExpiresAt: ns.ExpiresAt,
		}
		if ns.IP.IsValid() {
			ip := ns.IP.Unmap()
			p.CreatedIp = &ip
		}
		if ns.UserAgent != "" {
			p.UserAgent = &ns.UserAgent
		}
		return q.InsertSession(ctx, p)
	})
}

// SessionByHash reads a session and its user.
func (s *PGStore) SessionByHash(ctx context.Context, hash []byte) (sessionUser, error) {
	r, err := s.q.SessionWithUser(ctx, hash)
	if err != nil {
		return sessionUser{}, notFound(err)
	}
	return sessionUser{
		CSRFHash: r.CsrfHash, LastSeenAt: r.LastSeenAt, IdleExpiresAt: r.IdleExpiresAt, ExpiresAt: r.ExpiresAt,
		RevokedAt: r.RevokedAt,
		User:      toUser(r.ID, r.Uuid, r.Email, r.Name, r.Role, r.Status, r.LastLoginAt, r.CreatedAt),
	}, nil
}

// TouchSession slides the idle expiry.
func (s *PGStore) TouchSession(ctx context.Context, hash []byte, now, idle time.Time) error {
	return s.q.TouchSession(ctx, db.TouchSessionParams{TokenHash: hash, Now: now, IdleExpiresAt: idle})
}

// RevokeSession revokes one active session.
func (s *PGStore) RevokeSession(ctx context.Context, hash []byte, now time.Time, e audit.Entry) error {
	return s.tx(ctx, e, func(q *db.Queries) error {
		_, err := q.RevokeSession(ctx, db.RevokeSessionParams{TokenHash: hash, Now: &now})
		return notFound(err)
	})
}

// APIKeyByHash reads an API key and its user.
func (s *PGStore) APIKeyByHash(ctx context.Context, hash []byte) (apiKeyUser, error) {
	r, err := s.q.APIKeyWithUser(ctx, hash)
	if err != nil {
		return apiKeyUser{}, notFound(err)
	}
	return apiKeyUser{
		ID: r.KeyID, Hash: r.Hash, ExpiresAt: r.ExpiresAt, RevokedAt: r.RevokedAt, LastUsedAt: r.LastUsedAt,
		User: toUser(r.ID, r.Uuid, r.Email, r.Name, r.Role, r.Status, r.LastLoginAt, r.CreatedAt),
	}, nil
}

// TouchAPIKey records use.
func (s *PGStore) TouchAPIKey(ctx context.Context, id int64, now time.Time) error {
	return s.q.TouchAPIKey(ctx, db.TouchAPIKeyParams{ID: id, Now: &now})
}

// CreateUser inserts a user.
func (s *PGStore) CreateUser(ctx context.Context, email, name string, role Role, e audit.Entry) (User, error) {
	var u User
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := s.q.WithTx(tx).InsertUser(ctx, db.InsertUserParams{Email: email, Name: name, Role: string(role)})
		if err != nil {
			if database.IsCode(err, database.CodeUniqueViolation) {
				return apperr.New(apperr.Conflict, "a user with this email already exists")
			}
			return err
		}
		u = toUser(r.ID, r.Uuid, r.Email, r.Name, r.Role, r.Status, r.LastLoginAt, r.CreatedAt)
		e.EntityID = u.UUID.String()
		return s.auditor.Record(ctx, tx, e)
	})
	return u, err
}

// SetPassword stores the hash, links the password identity and revokes sessions.
func (s *PGStore) SetPassword(ctx context.Context, userID int64, email, hash string, now time.Time, e audit.Entry) error {
	return s.tx(ctx, e, func(q *db.Queries) error {
		if err := q.UpsertPassword(ctx, db.UpsertPasswordParams{UserID: userID, Hash: hash, Now: now}); err != nil {
			return err
		}
		if err := q.UpsertPasswordIdentity(ctx, db.UpsertPasswordIdentityParams{UserID: userID, Subject: email}); err != nil {
			return err
		}
		_, err := q.RevokeUserSessions(ctx, db.RevokeUserSessionsParams{UserID: userID, Now: &now})
		return err
	})
}

// SetRole changes the role and revokes sessions.
func (s *PGStore) SetRole(ctx context.Context, userID int64, role Role, now time.Time, e audit.Entry) error {
	return s.tx(ctx, e, func(q *db.Queries) error {
		n, err := q.SetUserRole(ctx, db.SetUserRoleParams{ID: userID, Role: string(role), Now: now})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		_, err = q.RevokeUserSessions(ctx, db.RevokeUserSessionsParams{UserID: userID, Now: &now})
		return err
	})
}

// SetStatus changes the status and revokes sessions.
func (s *PGStore) SetStatus(ctx context.Context, userID int64, status string, now time.Time, e audit.Entry) error {
	return s.tx(ctx, e, func(q *db.Queries) error {
		n, err := q.SetUserStatus(ctx, db.SetUserStatusParams{ID: userID, Status: status, Now: now})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		_, err = q.RevokeUserSessions(ctx, db.RevokeUserSessionsParams{UserID: userID, Now: &now})
		return err
	})
}

// InsertAPIKey stores a key hash.
func (s *PGStore) InsertAPIKey(ctx context.Context, k newAPIKeyRow, e audit.Entry) (APIKey, error) {
	var out APIKey
	err := s.tx(ctx, e, func(q *db.Queries) error {
		p := db.InsertAPIKeyParams{UserID: k.UserID, Prefix: k.Prefix, Hash: k.Hash, Now: k.Now, ExpiresAt: k.ExpiresAt}
		if k.Label != "" {
			p.Label = &k.Label
		}
		r, err := q.InsertAPIKey(ctx, p)
		if err != nil {
			return fmt.Errorf("auth: insert api key: %w", err)
		}
		out = APIKey{ID: r.Uuid, Prefix: r.Prefix, CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt, ExpiresAt: r.ExpiresAt, RevokedAt: r.RevokedAt}
		if r.Label != nil {
			out.Label = *r.Label
		}
		return nil
	})
	return out, err
}

// RevokeAPIKey revokes a key owned by userID.
func (s *PGStore) RevokeAPIKey(ctx context.Context, userID int64, id uuid.UUID, now time.Time, e audit.Entry) error {
	return s.tx(ctx, e, func(q *db.Queries) error {
		n, err := q.RevokeAPIKey(ctx, db.RevokeAPIKeyParams{Uuid: id, UserID: userID, Now: &now})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ListAPIKeys lists a user's keys, newest first.
func (s *PGStore) ListAPIKeys(ctx context.Context, userID int64) ([]APIKey, error) {
	rows, err := s.q.ListAPIKeys(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]APIKey, 0, len(rows))
	for _, r := range rows {
		k := APIKey{ID: r.Uuid, Prefix: r.Prefix, CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt, ExpiresAt: r.ExpiresAt, RevokedAt: r.RevokedAt}
		if r.Label != nil {
			k.Label = *r.Label
		}
		out = append(out, k)
	}
	return out, nil
}
