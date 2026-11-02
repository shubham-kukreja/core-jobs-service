-- name: GetIdempotency :one
SELECT method, path, request_hash, status_code, response_body
FROM app.idempotency_keys
WHERE user_id = @user_id AND key = @key AND expires_at > @now;

-- name: PutIdempotency :execrows
-- Inserts the record, replacing an expired one for the same key. Zero rows
-- means a live record exists (a concurrent request won the race).
INSERT INTO app.idempotency_keys (user_id, key, method, path, request_hash, status_code, response_body, created_at, expires_at)
VALUES (@user_id, @key, @method, @path, @request_hash, @status_code, @response_body, @now, @expires_at)
ON CONFLICT (user_id, key) DO UPDATE
SET method = EXCLUDED.method, path = EXCLUDED.path, request_hash = EXCLUDED.request_hash,
    status_code = EXCLUDED.status_code, response_body = EXCLUDED.response_body,
    created_at = EXCLUDED.created_at, expires_at = EXCLUDED.expires_at
WHERE app.idempotency_keys.expires_at <= @now;
