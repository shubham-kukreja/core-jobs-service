// Package admincli implements `app user …`, the only way to create users
// (there is no self-signup). Passwords are read from stdin, never from argv,
// so they stay out of shell history and process listings.
package admincli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/shubham-kukreja/core-jobs-service/internal/auth"
)

// Admin is the account surface the CLI needs; *auth.Service satisfies it.
type Admin interface {
	Create(ctx context.Context, email, name string, role auth.Role) (auth.User, error)
	ByEmail(ctx context.Context, email string) (auth.User, error)
	SetPassword(ctx context.Context, userID int64, password string) error
	CreateAPIKey(ctx context.Context, userID int64, label string, ttl time.Duration) (string, auth.APIKey, error)
}

// IO holds the CLI's streams. Stdin may be a terminal (*os.File), in which
// case passwords are read without echo.
type IO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// ErrUsage is returned for unknown commands or bad flags.
var ErrUsage = errors.New("usage")

// Usage describes the commands.
const Usage = `usage:
  app user create --email EMAIL --name NAME --role viewer|analyst|admin
  app user set-password --email EMAIL        (password read from stdin)
  app user create-api-key --email EMAIL [--label LABEL] [--ttl 720h]
`

// Run executes one `user` subcommand (args excludes "user").
func Run(ctx context.Context, a Admin, args []string, s IO) error {
	if len(args) == 0 {
		return ErrUsage
	}
	fs := flag.NewFlagSet("user "+args[0], flag.ContinueOnError)
	fs.SetOutput(s.Stderr)
	email := fs.String("email", "", "user email")
	switch args[0] {
	case "create":
		name := fs.String("name", "", "display name")
		role := fs.String("role", "", "viewer, analyst or admin")
		if err := parse(fs, args[1:]); err != nil {
			return err
		}
		u, err := a.Create(ctx, *email, *name, auth.Role(*role))
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(s.Stdout, "created user %s (%s, %s)\nset a password with: app user set-password --email %s\n",
			u.UUID, u.Email, u.Role, u.Email)
		return err

	case "set-password":
		if err := parse(fs, args[1:]); err != nil {
			return err
		}
		u, err := a.ByEmail(ctx, *email)
		if err != nil {
			return fmt.Errorf("user %q: %w", *email, err)
		}
		pw, err := readPassword(s)
		if err != nil {
			return err
		}
		if err := a.SetPassword(ctx, u.ID, pw); err != nil {
			return err
		}
		_, err = fmt.Fprintln(s.Stdout, "password set; existing sessions revoked")
		return err

	case "create-api-key":
		label := fs.String("label", "", "what the key is for")
		ttl := fs.Duration("ttl", 0, "lifetime, e.g. 720h (0 = no expiry)")
		if err := parse(fs, args[1:]); err != nil {
			return err
		}
		u, err := a.ByEmail(ctx, *email)
		if err != nil {
			return fmt.Errorf("user %q: %w", *email, err)
		}
		secret, k, err := a.CreateAPIKey(ctx, u.ID, *label, *ttl)
		if err != nil {
			return err
		}
		// The secret goes to stdout only (so it can be piped to a secret
		// store); the note goes to stderr.
		if _, err := fmt.Fprintf(s.Stderr, "API key %s created (prefix %s). It is shown once; store it now.\n", k.ID, k.Prefix); err != nil {
			return err
		}
		_, err = fmt.Fprintln(s.Stdout, secret)
		return err
	}
	return ErrUsage
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q (passwords are read from stdin)", ErrUsage, fs.Arg(0))
	}
	return nil
}

// readPassword reads without echo, twice, from a terminal; otherwise it
// reads one line, so it can be piped from a secret manager.
func readPassword(s IO) (string, error) {
	if f, ok := s.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) { //nolint:gosec // fd fits in int.
		fd := int(f.Fd()) //nolint:gosec // fd fits in int.
		_, _ = fmt.Fprint(s.Stderr, "New password: ")
		a, err := term.ReadPassword(fd)
		_, _ = fmt.Fprintln(s.Stderr)
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprint(s.Stderr, "Repeat password: ")
		b, err := term.ReadPassword(fd)
		_, _ = fmt.Fprintln(s.Stderr)
		if err != nil {
			return "", err
		}
		if string(a) != string(b) {
			return "", errors.New("passwords do not match")
		}
		return string(a), nil
	}
	line, err := bufio.NewReader(io.LimitReader(s.Stdin, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	pw := strings.TrimRight(line, "\r\n")
	if pw == "" {
		return "", errors.New("no password on stdin")
	}
	return pw, nil
}
