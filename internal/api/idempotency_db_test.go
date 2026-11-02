package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/testdb"
)

func TestPGIdempotency(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	var uid int64
	if err := db.Owner.QueryRow(ctx, `INSERT INTO auth.users(email,name,role) VALUES ('a@b.co','A','admin') RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	store := NewPGIdempotency(db.Runtime)
	now := time.Now().UTC()
	rec := IdemRecord{UserID: uid, Key: "k1", Method: "POST", Path: "/v1/datapoints", RequestHash: make([]byte, 32),
		Status: 201, Body: []byte(`{"ok":true}`), Now: now, ExpiresAt: now.Add(IdempotencyTTL)}

	put := func(r IdemRecord) error {
		return database.InTx(ctx, db.Runtime, func(tx pgx.Tx) error { return store.Put(ctx, tx, r) })
	}
	if err := put(rec); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, uid, "k1", now)
	if err != nil || got == nil || got.Status != 201 || string(got.Body) != `{"ok": true}` && string(got.Body) != `{"ok":true}` {
		t.Fatalf("get: %+v %v", got, err)
	}
	if err := put(rec); !errors.Is(err, errIdemRace) {
		t.Fatalf("live duplicate: %v", err)
	}
	// After expiry the key is free again.
	later := now.Add(IdempotencyTTL + time.Minute)
	if got, _ := store.Get(ctx, uid, "k1", later); got != nil {
		t.Fatal("expired record returned")
	}
	rec.Now, rec.ExpiresAt = later, later.Add(IdempotencyTTL)
	if err := put(rec); err != nil {
		t.Fatalf("reuse after expiry: %v", err)
	}
}
