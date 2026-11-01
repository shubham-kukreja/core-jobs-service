-- +goose Up
-- Users, credentials, sessions and API keys.
--
-- Login is modelled as identities so OAuth/SSO providers can be added later as
-- new rows (provider = 'google', subject = their stable id) without changing
-- users. Password credentials live in their own table, keyed by user, so a
-- user can exist with SSO only.
--
-- No secret is stored in plaintext: passwords are argon2id PHC strings,
-- session tokens and API keys are stored as SHA-256 of a 256-bit random token.

CREATE TABLE auth.users (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid            uuid UNIQUE NOT NULL DEFAULT gen_random_uuid(),
  email           citext UNIQUE NOT NULL CHECK (length(email) BETWEEN 3 AND 254 AND email LIKE '%_@_%'),
  name            text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
  role            text NOT NULL CHECK (role IN ('viewer','analyst','admin')),
  status          text NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
  failed_logins   int NOT NULL DEFAULT 0 CHECK (failed_logins >= 0),
  locked_until    timestamptz,
  last_login_at   timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE auth.identities (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id     bigint NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  provider    text NOT NULL CHECK (provider ~ '^[a-z][a-z0-9_]{1,31}$'),   -- 'password', later 'google', 'okta'
  subject     text NOT NULL CHECK (length(subject) BETWEEN 1 AND 255),    -- provider's stable user id; email for 'password'
  created_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (provider, subject),
  UNIQUE (user_id, provider)
);

CREATE TABLE auth.password_credentials (
  user_id     bigint PRIMARY KEY REFERENCES auth.users(id) ON DELETE CASCADE,
  hash        text NOT NULL CHECK (hash LIKE '$argon2id$%'),
  updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE auth.sessions (
  token_hash      bytea PRIMARY KEY CHECK (length(token_hash) = 32),
  user_id         bigint NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  csrf_hash       bytea NOT NULL CHECK (length(csrf_hash) = 32),
  created_at      timestamptz NOT NULL DEFAULT now(),
  last_seen_at    timestamptz NOT NULL DEFAULT now(),
  idle_expires_at timestamptz NOT NULL,
  expires_at      timestamptz NOT NULL,                -- absolute lifetime
  revoked_at      timestamptz,
  -- Security monitoring only; purged with the session (see retention in docs/security.md).
  created_ip      inet,
  user_agent      text CHECK (length(user_agent) <= 512),
  CHECK (idle_expires_at <= expires_at)
);
CREATE INDEX sessions_user ON auth.sessions (user_id) WHERE revoked_at IS NULL;
CREATE INDEX sessions_expiry ON auth.sessions (expires_at);

CREATE TABLE auth.api_keys (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid          uuid UNIQUE NOT NULL DEFAULT gen_random_uuid(),
  user_id       bigint NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  prefix        text NOT NULL CHECK (prefix ~ '^[A-Za-z0-9_]{4,16}$'),   -- shown in UI to identify a key
  hash          bytea UNIQUE NOT NULL CHECK (length(hash) = 32),
  label         text CHECK (length(label) <= 100),
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_used_at  timestamptz,
  expires_at    timestamptz,
  revoked_at    timestamptz
);
CREATE INDEX api_keys_user ON auth.api_keys (user_id);

-- +goose Down
DROP TABLE auth.api_keys, auth.sessions, auth.password_credentials, auth.identities, auth.users;
