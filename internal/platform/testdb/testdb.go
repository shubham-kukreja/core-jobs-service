// Package testdb gives repository tests a fresh, fully migrated Postgres
// database. Tests skip when TEST_DATABASE_URL is unset, so unit tests still
// run without Docker; CI always sets it.
//
// TEST_DATABASE_URL must point at a superuser (owner) connection on any
// database; each call creates and later drops its own database.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/migrate"
)

// DB is a migrated test database.
type DB struct {
	// Owner connects as the migration owner (DDL, bypasses grants).
	Owner *pgxpool.Pool
	// Runtime connects with exactly the app_runtime privileges the api and
	// worker have in production. Repository tests should use this pool.
	Runtime *pgxpool.Pool
	URL     string
}

// New creates a migrated database for t and drops it when t finishes.
func New(t testing.TB) *DB {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("testdb: connect: %v", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()

	name := "test_" + randomSuffix(t)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("testdb: create database: %v", err)
	}
	t.Cleanup(func() { drop(base, name) })

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("testdb: parse url: %v", err)
	}
	u.Path = "/" + name
	dbURL := u.String()

	owner, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("testdb: owner pool: %v", err)
	}
	t.Cleanup(owner.Close)
	if err := migrate.Up(ctx, owner, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("testdb: migrate: %v", err)
	}

	rcfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("testdb: runtime config: %v", err)
	}
	rcfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE app_runtime")
		return err
	}
	runtime, err := pgxpool.NewWithConfig(ctx, rcfg)
	if err != nil {
		t.Fatalf("testdb: runtime pool: %v", err)
	}
	t.Cleanup(runtime.Close)

	return &DB{Owner: owner, Runtime: runtime, URL: dbURL}
}

func drop(base, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		return
	}
	defer func() { _ = admin.Close(context.Background()) }()
	_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
}

func randomSuffix(t testing.TB) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("testdb: random: %v", err)
	}
	return hex.EncodeToString(b)
}
