package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/config"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/logging"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/migrate"
)

func runMigrate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) != 1 {
		return errUsage
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log := logging.New(stderr, cfg.LogLevel).With("app", "core-migrate")
	pool, err := database.Open(ctx, cfg.MigrationDatabaseURL.Reveal(), database.Options{
		MaxConns: 2, AppName: "core-migrate", StatementTimeout: 10 * time.Minute,
	})
	if err != nil {
		return err
	}
	defer pool.Close()

	switch args[0] {
	case "up":
		return migrate.Up(ctx, pool, log)
	case "status":
		v, err := migrate.Status(ctx, pool)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "schema version %d\n", v)
		return err
	case "down-all":
		if cfg.IsProduction() {
			return errors.New("migrate down-all is disabled in staging and production")
		}
		return migrate.DownAll(ctx, pool)
	default:
		return errUsage
	}
}
