package datapoints

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
)

const (
	sqlClaimOutbox = `
SELECT id, topic, payload, created_at FROM dp.outbox
WHERE dispatched_at IS NULL ORDER BY id LIMIT $1
FOR UPDATE SKIP LOCKED`
	sqlMarkDispatched = `UPDATE dp.outbox SET dispatched_at = now() WHERE id = ANY($1::bigint[]) AND dispatched_at IS NULL`
	sqlInsertRaw      = `
INSERT INTO dp.raw_payloads (source_id, ingest_run_id, fetched_at, request, blob_uri, sha256, bytes)
VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`
)

// MaxClaim bounds one outbox claim.
const MaxClaim = 1000

func (s *Store) Claim(ctx context.Context, tx pgx.Tx, limit int) ([]OutboxEvent, error) {
	if limit <= 0 || limit > MaxClaim {
		limit = MaxClaim
	}
	rows, err := tx.Query(ctx, sqlClaimOutbox, limit)
	if err != nil {
		return nil, fmt.Errorf("datapoints: claim outbox: %w", err)
	}
	defer rows.Close()
	var out []OutboxEvent
	for rows.Next() {
		var ev OutboxEvent
		if err := rows.Scan(&ev.ID, &ev.Topic, &ev.Payload, &ev.CreatedAt); err != nil {
			return nil, fmt.Errorf("datapoints: scan outbox: %w", err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *Store) MarkDispatched(ctx context.Context, tx pgx.Tx, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, sqlMarkDispatched, ids); err != nil {
		return fmt.Errorf("datapoints: mark dispatched: %w", err)
	}
	return nil
}

// sensitiveRequestKeys must never appear in stored request metadata: a
// connector that forgets to strip credentials fails loudly instead of
// persisting them.
var sensitiveRequestKeys = []string{"api_key", "apikey", "access_key", "token", "secret", "password", "authorization", "cookie", "credential", "signature"}

func (s *Store) Record(ctx context.Context, tx pgx.Tx, p RawPayload) (int64, error) {
	if !strings.HasPrefix(p.BlobURI, "blob://") || len(p.SHA256) != 32 || p.Bytes < 0 || p.FetchedAt.IsZero() {
		return 0, apperr.New(apperr.InvalidParams, "raw payload metadata is incomplete")
	}
	if k := findSensitiveKey(p.Request); k != "" {
		return 0, apperr.New(apperr.InvalidParams, fmt.Sprintf("raw payload request contains sensitive field %q", k))
	}
	src, err := s.cat.Source(ctx, p.SourceKey)
	if err != nil {
		return 0, err
	}
	req, err := json.Marshal(nonNilMap(p.Request))
	if err != nil {
		return 0, fmt.Errorf("datapoints: raw request: %w", err)
	}
	var id int64
	if err := tx.QueryRow(ctx, sqlInsertRaw, src.ID, p.IngestRunID, p.FetchedAt.UTC(), req, p.BlobURI, p.SHA256, p.Bytes).Scan(&id); err != nil {
		return 0, fmt.Errorf("datapoints: record raw payload: %w", err)
	}
	return id, nil
}

// findSensitiveKey returns the first key (at any depth) that looks like a
// credential, or "".
func findSensitiveKey(v any) string {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			lk := strings.ToLower(k)
			if lk == "key" {
				return k
			}
			for _, s := range sensitiveRequestKeys {
				if strings.Contains(lk, s) {
					return k
				}
			}
			if f := findSensitiveKey(x); f != "" {
				return f
			}
		}
	case []any:
		for _, x := range t {
			if f := findSensitiveKey(x); f != "" {
				return f
			}
		}
	}
	return ""
}

func nonNilMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
