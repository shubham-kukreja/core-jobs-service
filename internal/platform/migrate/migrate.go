// Package migrate applies schema migrations: our goose SQL files, then the
// queue engine's own tables in the `river` schema. It must run with the owner
// (DDL) role, never the runtime role.
package migrate

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/shubham-kukreja/core-jobs-service/migrations"
)

// RiverSchema holds the queue engine's tables.
const RiverSchema = "river"

// Up applies every pending migration.
func Up(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	p, closeDB, err := provider(pool)
	if err != nil {
		return err
	}
	defer closeDB()
	results, err := p.Up(ctx)
	for _, r := range results {
		log.Info("migration applied", "version", r.Source.Version, "file", r.Source.Path, "duration", r.Duration)
	}
	if err != nil {
		return fmt.Errorf("migrate: goose up: %w", err)
	}
	if err := riverUp(ctx, pool); err != nil {
		return err
	}
	return grantRiver(ctx, pool)
}

// DownAll reverts every migration. Development and test use only.
func DownAll(ctx context.Context, pool *pgxpool.Pool) error {
	m, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Schema: RiverSchema})
	if err != nil {
		return fmt.Errorf("migrate: river: %w", err)
	}
	if _, err := m.Migrate(ctx, rivermigrate.DirectionDown, &rivermigrate.MigrateOpts{TargetVersion: -1}); err != nil {
		return fmt.Errorf("migrate: river down: %w", err)
	}
	p, closeDB, err := provider(pool)
	if err != nil {
		return err
	}
	defer closeDB()
	if _, err := p.DownTo(ctx, 0); err != nil {
		return fmt.Errorf("migrate: goose down: %w", err)
	}
	return nil
}

// Status returns the current goose schema version.
func Status(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	p, closeDB, err := provider(pool)
	if err != nil {
		return 0, err
	}
	defer closeDB()
	return p.GetDBVersion(ctx)
}

func provider(pool *pgxpool.Pool) (*goose.Provider, func(), error) {
	db := stdlib.OpenDBFromPool(pool)
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("migrate: goose: %w", err)
	}
	return p, func() { _ = db.Close() }, nil
}

func riverUp(ctx context.Context, pool *pgxpool.Pool) error {
	m, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Schema: RiverSchema})
	if err != nil {
		return fmt.Errorf("migrate: river: %w", err)
	}
	if _, err := m.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("migrate: river up: %w", err)
	}
	return nil
}

// grantRiver gives app_runtime DML on River's tables. It is re-applied after
// every migrate because River may add tables in new versions.
func grantRiver(ctx context.Context, pool *pgxpool.Pool) error {
	const q = `
GRANT USAGE ON SCHEMA river TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA river TO app_runtime;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA river TO app_runtime;
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA river TO app_runtime;`
	if _, err := pool.Exec(ctx, q); err != nil {
		return fmt.Errorf("migrate: river grants: %w", err)
	}
	return nil
}
