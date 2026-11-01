-- +goose Up
-- Notes and API idempotency records.

CREATE TABLE app.notes (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uuid        uuid UNIQUE NOT NULL DEFAULT gen_random_uuid(),
  title       text NOT NULL CHECK (length(title) BETWEEN 1 AND 200),
  -- Stored as written. Clients must render with a sanitising Markdown
  -- renderer (no raw HTML); the API never returns rendered HTML.
  body_md     text NOT NULL CHECK (length(body_md) <= 100000),
  author_id   bigint NOT NULL REFERENCES auth.users(id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  deleted_at  timestamptz
);

CREATE TABLE app.note_refs (
  note_id   bigint NOT NULL REFERENCES app.notes(id) ON DELETE CASCADE,
  ref_kind  text NOT NULL CHECK (ref_kind IN ('series','node','parameter','version','backtest')),
  ref_key   text NOT NULL CHECK (length(ref_key) <= 160),
  PRIMARY KEY (note_id, ref_kind, ref_key)
);
CREATE INDEX note_refs_target ON app.note_refs (ref_kind, ref_key);

-- Replays of POSTs carrying an Idempotency-Key. A key reused with a different
-- request body returns 422 idempotency_mismatch.
CREATE TABLE app.idempotency_keys (
  user_id        bigint NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  key            text NOT NULL CHECK (length(key) BETWEEN 1 AND 128),
  method         text NOT NULL,
  path           text NOT NULL CHECK (length(path) <= 512),
  request_hash   bytea NOT NULL CHECK (length(request_hash) = 32),
  status_code    int,
  response_body  jsonb,
  created_at     timestamptz NOT NULL DEFAULT now(),
  expires_at     timestamptz NOT NULL,
  PRIMARY KEY (user_id, key)
);
CREATE INDEX idempotency_expiry ON app.idempotency_keys (expires_at);

-- +goose Down
DROP TABLE app.idempotency_keys, app.note_refs, app.notes;
