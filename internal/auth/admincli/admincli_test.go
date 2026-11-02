package admincli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/shubham-kukreja/core-jobs-service/internal/auth"
)

type fakeAdmin struct {
	created  auth.User
	password string
}

func (f *fakeAdmin) Create(_ context.Context, email, name string, role auth.Role) (auth.User, error) {
	f.created = auth.User{ID: 1, UUID: uuid.New(), Email: email, Name: name, Role: role}
	return f.created, nil
}

func (f *fakeAdmin) ByEmail(_ context.Context, email string) (auth.User, error) {
	if email != "a@b.co" {
		return auth.User{}, auth.ErrNotFound
	}
	return auth.User{ID: 1, Email: email}, nil
}

func (f *fakeAdmin) SetPassword(_ context.Context, _ int64, pw string) error {
	f.password = pw
	return nil
}

func (f *fakeAdmin) CreateAPIKey(context.Context, int64, string, time.Duration) (string, auth.APIKey, error) {
	return "core_deadbeef_secret", auth.APIKey{ID: uuid.New(), Prefix: "deadbeef"}, nil
}

func run(t *testing.T, a Admin, stdin string, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := Run(context.Background(), a, args, IO{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errOut})
	return out.String(), errOut.String(), err
}

func TestCreate(t *testing.T) {
	f := &fakeAdmin{}
	out, _, err := run(t, f, "", "create", "--email", "a@b.co", "--name", "A", "--role", "admin")
	if err != nil || f.created.Role != auth.RoleAdmin || !strings.Contains(out, "created user") {
		t.Fatalf("%q %v", out, err)
	}
}

func TestSetPasswordFromStdinNotArgv(t *testing.T) {
	f := &fakeAdmin{}
	if _, _, err := run(t, f, "tidal-saffron-lantern-41\n", "set-password", "--email", "a@b.co"); err != nil {
		t.Fatal(err)
	}
	if f.password != "tidal-saffron-lantern-41" {
		t.Fatalf("password %q", f.password)
	}
	if _, _, err := run(t, f, "", "set-password", "--email", "a@b.co", "--password", "x"); !errors.Is(err, ErrUsage) {
		t.Fatalf("password flag must be rejected: %v", err)
	}
	if _, _, err := run(t, f, "", "set-password", "--email", "a@b.co", "positional-password"); !errors.Is(err, ErrUsage) {
		t.Fatalf("positional password must be rejected: %v", err)
	}
	if _, _, err := run(t, f, "", "set-password", "--email", "a@b.co"); err == nil {
		t.Fatal("empty stdin accepted")
	}
}

func TestCreateAPIKeyPrintsSecretOnlyToStdout(t *testing.T) {
	out, errOut, err := run(t, &fakeAdmin{}, "", "create-api-key", "--email", "a@b.co", "--label", "ci")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "core_deadbeef_secret" || strings.Contains(errOut, "secret") {
		t.Fatalf("stdout %q stderr %q", out, errOut)
	}
}

func TestUnknown(t *testing.T) {
	if _, _, err := run(t, &fakeAdmin{}, "", "delete"); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
	if _, _, err := run(t, &fakeAdmin{}, ""); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
}
