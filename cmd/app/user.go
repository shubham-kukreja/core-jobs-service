package main

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/shubham-kukreja/core-jobs-service/internal/app"
	"github.com/shubham-kukreja/core-jobs-service/internal/auth/admincli"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
)

// runUser administers accounts. Passwords are read from stdin (without echo
// on a terminal), never from arguments.
func runUser(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, _, pool, err := bootstrap(ctx, stderr, "core-admin")
	if err != nil {
		return err
	}
	defer pool.Close()
	svc, _, err := app.NewAuth(pool, cfg, clock.System{})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	return admincli.Run(ctx, svc, args, admincli.IO{Stdin: os.Stdin, Stdout: stdout, Stderr: stderr})
}
