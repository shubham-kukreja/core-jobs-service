package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/api/internal/db"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
)

// IdempotencyTTL is how long a key replays its first response.
const IdempotencyTTL = 24 * time.Hour

var idempotencyKeyRE = regexp.MustCompile(`^[A-Za-z0-9_.:\-]{1,128}$`)

// IdemRecord is a stored response for (user, key).
type IdemRecord struct {
	UserID      int64
	Key         string
	Method      string
	Path        string
	RequestHash []byte
	Status      int
	Body        []byte
	Now         time.Time
	ExpiresAt   time.Time
}

// errIdemRace means another request stored the same key first.
var errIdemRace = errors.New("api: idempotency key already in use")

// IdempotencyStore persists idempotent responses.
type IdempotencyStore interface {
	// Get returns the live record or nil.
	Get(ctx context.Context, userID int64, key string, now time.Time) (*IdemRecord, error)
	// Put stores rec in tx; errIdemRace if a live record exists.
	Put(ctx context.Context, tx pgx.Tx, rec IdemRecord) error
}

// PGIdempotency stores records in app.idempotency_keys.
type PGIdempotency struct{ q *db.Queries }

// NewPGIdempotency returns the Postgres store.
func NewPGIdempotency(conn database.DBTX) *PGIdempotency { return &PGIdempotency{q: db.New(conn)} }

// Get implements IdempotencyStore.
func (p *PGIdempotency) Get(ctx context.Context, userID int64, key string, now time.Time) (*IdemRecord, error) {
	r, err := p.q.GetIdempotency(ctx, db.GetIdempotencyParams{UserID: userID, Key: key, Now: now})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rec := &IdemRecord{UserID: userID, Key: key, Method: r.Method, Path: r.Path, RequestHash: r.RequestHash, Body: r.ResponseBody}
	if r.StatusCode != nil {
		rec.Status = int(*r.StatusCode)
	}
	return rec, nil
}

// Put implements IdempotencyStore.
func (p *PGIdempotency) Put(ctx context.Context, tx pgx.Tx, rec IdemRecord) error {
	status := int32(rec.Status) //nolint:gosec // HTTP status codes fit in int32.
	n, err := p.q.WithTx(tx).PutIdempotency(ctx, db.PutIdempotencyParams{
		UserID: rec.UserID, Key: rec.Key, Method: rec.Method, Path: rec.Path, RequestHash: rec.RequestHash,
		StatusCode: &status, ResponseBody: rec.Body, Now: rec.Now, ExpiresAt: rec.ExpiresAt,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return errIdemRace
	}
	return nil
}

// txResult is what a mutating handler produces inside its transaction.
type txResult struct {
	Status int
	Body   any
}

// mutate runs fn in one transaction (the change plus its audit row). With an
// Idempotency-Key header the response is stored in the same transaction and
// replayed for retries; the same key with a different request is rejected.
func (s *Server) mutate(w http.ResponseWriter, r *http.Request, body []byte, fn func(tx pgx.Tx) (txResult, error)) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		var res txResult
		err := database.InTx(r.Context(), s.db, func(tx pgx.Tx) error {
			var err error
			res, err = fn(tx)
			return err
		})
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		writeJSON(w, res.Status, res.Body)
		return
	}
	if !idempotencyKeyRE.MatchString(key) {
		s.writeError(w, r, apperr.New(apperr.ValidationFailed, "Invalid Idempotency-Key", apperr.Detail{Field: "Idempotency-Key", Reason: "invalid_format"}))
		return
	}
	p := mustPrincipal(r.Context())
	now := s.clock.Now()
	hash := requestHash(r, body)

	prev, err := s.idem.Get(r.Context(), p.UserID, key, now)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if prev != nil {
		if prev.Method != r.Method || prev.Path != r.URL.Path || subtle.ConstantTimeCompare(prev.RequestHash, hash) != 1 {
			s.writeError(w, r, apperr.New(apperr.IdempotencyMismatch, "Idempotency-Key was used with a different request"))
			return
		}
		w.Header().Set("Idempotent-Replayed", "true")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(prev.Status)
		_, _ = w.Write(prev.Body)
		return
	}

	var res txResult
	var encoded []byte
	err = database.InTx(r.Context(), s.db, func(tx pgx.Tx) error {
		var err error
		if res, err = fn(tx); err != nil {
			return err
		}
		if encoded, err = json.Marshal(res.Body); err != nil {
			return err
		}
		return s.idem.Put(r.Context(), tx, IdemRecord{
			UserID: p.UserID, Key: key, Method: r.Method, Path: r.URL.Path, RequestHash: hash,
			Status: res.Status, Body: encoded, Now: now, ExpiresAt: now.Add(IdempotencyTTL),
		})
	})
	if errors.Is(err, errIdemRace) {
		s.writeError(w, r, apperr.New(apperr.Conflict, "A request with this Idempotency-Key is already being processed"))
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(res.Status)
	_, _ = w.Write(append(encoded, '\n'))
}

// requestHash binds a key to method, path and body bytes.
func requestHash(r *http.Request, body []byte) []byte {
	h := sha256.New()
	h.Write([]byte(r.Method))
	h.Write([]byte{0})
	h.Write([]byte(r.URL.Path))
	h.Write([]byte{0})
	h.Write(bytes.TrimSpace(body))
	return h.Sum(nil)
}
