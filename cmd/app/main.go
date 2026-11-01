// Command app is the single binary for every process role:
//
//	app api                 serve the HTTP API
//	app worker              run job workers and the scheduler
//	app migrate up|status   apply schema migrations (owner role)
//	app migrate down-all    revert everything (development/test only)
//	app seed validate DIR   validate seed YAML against its JSON Schemas
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const usage = `usage: app <command> [args]

commands:
  api                    serve the HTTP API
  worker                 run job workers and the scheduler
  migrate up|status      apply schema migrations (uses MIGRATION_DATABASE_URL)
  migrate down-all       revert all migrations (development/test only)
  seed validate <dir>    validate seed files against configs/schema
`

func main() { os.Exit(exitCode()) }

// exitCode runs the command and maps its error to a process exit code. It is
// separate from main so deferred cleanup runs before os.Exit.
func exitCode() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		_, _ = fmt.Fprint(os.Stderr, usage)
		return 2
	default:
		_, _ = fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
}

var errUsage = errors.New("usage")

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errUsage
	}
	switch args[0] {
	case "api":
		return runAPI(ctx, stderr)
	case "worker":
		return runWorker(ctx, stderr)
	case "migrate":
		return runMigrate(ctx, args[1:], stdout, stderr)
	case "seed":
		return runSeed(args[1:], stdout)
	case "-h", "--help", "help":
		_, err := fmt.Fprint(stdout, usage)
		return err
	default:
		return errUsage
	}
}
