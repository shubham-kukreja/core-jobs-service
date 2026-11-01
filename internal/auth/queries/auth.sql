-- ---------------------------------------------------------------- users

-- name: InsertUser :one
INSERT INTO auth.users (email, name, role)
VALUES (@email, @name, @role)
RETURNING id, uuid, email, name, role, status, last_login_at, created_at;

-- name: GetUser :one
SELECT id, uuid, email, name, role, status, last_login_at, created_at
FROM auth.users WHERE id = @id;

-- name: GetUserByEmail :one
SELECT id, uuid, email, name, role, status, last_login_at, created_at
FROM auth.users WHERE email = @email;

-- name: UserForLogin :one
-- The password hash is read only here, for verification.
SELECT u.id, u.uuid, u.email, u.name, u.role, u.status, u.last_login_at, u.created_at,
       u.failed_logins, u.locked_until, pc.hash AS password_hash
FROM auth.users u
LEFT JOIN auth.password_credentials pc ON pc.user_id = u.id
WHERE u.email = @email;

-- name: SetLoginFailure :exec
UPDATE auth.users
SET failed_logins = @failed_logins, locked_until = sqlc.narg('locked_until'), updated_at = @now
WHERE id = @id;

-- name: SetLoginSuccess :exec
UPDATE auth.users
SET failed_logins = 0, locked_until = NULL, last_login_at = @now, updated_at = @now
WHERE id = @id;

-- name: SetUserRole :execrows
UPDATE auth.users SET role = @role, updated_at = @now WHERE id = @id;

-- name: SetUserStatus :execrows
UPDATE auth.users SET status = @status, updated_at = @now WHERE id = @id;

-- ---------------------------------------------------------------- credentials

-- name: UpsertPassword :exec
INSERT INTO auth.password_credentials (user_id, hash, updated_at)
VALUES (@user_id, @hash, @now)
ON CONFLICT (user_id) DO UPDATE SET hash = EXCLUDED.hash, updated_at = EXCLUDED.updated_at;

-- name: UpsertPasswordIdentity :exec
INSERT INTO auth.identities (user_id, provider, subject)
VALUES (@user_id, 'password', @subject)
ON CONFLICT (user_id, provider) DO UPDATE SET subject = EXCLUDED.subject;

-- ---------------------------------------------------------------- sessions

-- name: InsertSession :exec
INSERT INTO auth.sessions (token_hash, user_id, csrf_hash, created_at, last_seen_at, idle_expires_at, expires_at, created_ip, user_agent)
VALUES (@token_hash, @user_id, @csrf_hash, @now, @now, @idle_expires_at, @expires_at, sqlc.narg('created_ip'), sqlc.narg('user_agent'));

-- name: SessionWithUser :one
SELECT s.token_hash, s.csrf_hash, s.last_seen_at, s.idle_expires_at, s.expires_at, s.revoked_at,
       u.id, u.uuid, u.email, u.name, u.role, u.status, u.last_login_at, u.created_at
FROM auth.sessions s
JOIN auth.users u ON u.id = s.user_id
WHERE s.token_hash = @token_hash;

-- name: TouchSession :exec
UPDATE auth.sessions
SET last_seen_at = @now, idle_expires_at = @idle_expires_at
WHERE token_hash = @token_hash AND revoked_at IS NULL;

-- name: RevokeSession :one
UPDATE auth.sessions SET revoked_at = @now
WHERE token_hash = @token_hash AND revoked_at IS NULL
RETURNING user_id;

-- name: RevokeUserSessions :execrows
UPDATE auth.sessions SET revoked_at = @now
WHERE user_id = @user_id AND revoked_at IS NULL;

-- ---------------------------------------------------------------- API keys

-- name: InsertAPIKey :one
INSERT INTO auth.api_keys (user_id, prefix, hash, label, created_at, expires_at)
VALUES (@user_id, @prefix, @hash, sqlc.narg('label'), @now, sqlc.narg('expires_at'))
RETURNING uuid, prefix, label, created_at, last_used_at, expires_at, revoked_at;

-- name: APIKeyWithUser :one
SELECT k.id AS key_id, k.hash, k.expires_at, k.revoked_at, k.last_used_at,
       u.id, u.uuid, u.email, u.name, u.role, u.status, u.last_login_at, u.created_at
FROM auth.api_keys k
JOIN auth.users u ON u.id = k.user_id
WHERE k.hash = @hash;

-- name: TouchAPIKey :exec
UPDATE auth.api_keys SET last_used_at = @now WHERE id = @id;

-- name: RevokeAPIKey :execrows
UPDATE auth.api_keys SET revoked_at = @now
WHERE uuid = @uuid AND user_id = @user_id AND revoked_at IS NULL;

-- name: ListAPIKeys :many
SELECT uuid, prefix, label, created_at, last_used_at, expires_at, revoked_at
FROM auth.api_keys WHERE user_id = @user_id
ORDER BY created_at DESC;
