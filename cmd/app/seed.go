package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/shubham-kukreja/core-jobs-service/internal/app"
	"github.com/shubham-kukreja/core-jobs-service/internal/auth"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/seed"
)

// runSeed validates seed files, or applies them to the database as an admin:
//
//	app seed validate DIR
//	app seed apply DIR --as EMAIL
func runSeed(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	switch {
	case len(args) == 2 && args[0] == "validate":
		docs, err := seed.Validate(args[1])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "%d seed files valid\n", len(docs))
		return err
	case len(args) == 4 && args[0] == "apply" && args[2] == "--as":
		return applySeed(ctx, args[1], args[3], stdout, stderr)
	}
	return errUsage
}

func applySeed(ctx context.Context, dir, email string, stdout, stderr io.Writer) error {
	docs, err := seed.Validate(dir)
	if err != nil {
		return err
	}
	cfg, log, pool, err := bootstrap(ctx, stderr, "core-admin")
	if err != nil {
		return err
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	users, _, err := app.NewAuth(pool, cfg, clock.System{})
	if err != nil {
		return err
	}
	actor, err := users.ByEmail(ctx, email)
	if errors.Is(err, auth.ErrNotFound) || (err == nil && (actor.Status != "active" || actor.Role != auth.RoleAdmin)) {
		return errors.New("seed apply: --as must name an active admin account")
	}
	if err != nil {
		return err
	}
	core, err := app.NewCore(pool, log, clock.System{})
	if err != nil {
		return err
	}
	rep, err := seed.Apply(ctx, core.SeedTarget(), docs, actor.ID)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "applied %d sources, %d entities, %d series, %d import mappings; %d series queued for rebuild\n",
		rep.Sources, rep.Entities, rep.Series, rep.Mappings, len(rep.Rebuild))
	return err
}
