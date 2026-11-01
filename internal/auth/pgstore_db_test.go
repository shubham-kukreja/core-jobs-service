package auth_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/shubham-kukreja/core-jobs-service/internal/audit"
	"github.com/shubham-kukreja/core-jobs-service/internal/auth"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/testdb"
)

// TestPGFlow runs the service against Postgres with production privileges.
func TestPGFlow(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	auditor := audit.NewPG(db.Runtime)
	h, err := auth.NewHasher(auth.Argon2Params{MemoryKiB: 1024, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32}, 2)
	if err != nil {
		t.Fatal(err)
	}
	c := clock.NewFake(time.Now().UTC())
	s, err := auth.NewService(auth.NewPGStore(db.Runtime, auditor), h, auth.NewPolicy(), c, auth.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	u, err := s.Create(ctx, "riya@arviawealth.com", "Riya", auth.RoleAnalyst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, "RIYA@arviawealth.com", "Dup", auth.RoleViewer); err == nil {
		t.Fatal("duplicate email (case-insensitive) accepted")
	}
	const pw = "tidal-saffron-lantern-41"
	if err := s.SetPassword(ctx, u.ID, pw); err != nil {
		t.Fatal(err)
	}
	ci := auth.ClientInfo{IP: netip.MustParseAddr("203.0.113.7"), UserAgent: "test"}

	if _, err := s.Login(ctx, "riya@arviawealth.com", "wrong-password-1", ci); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := s.Login(ctx, "nobody@arviawealth.com", pw, ci); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("unknown email: %v", err)
	}
	sess, err := s.Login(ctx, "riya@arviawealth.com", pw, ci)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Session(ctx, sess.Token)
	if err != nil || p.UserID != u.ID {
		t.Fatalf("session: %+v %v", p, err)
	}
	c.Advance(2 * time.Minute)
	if _, err := s.Session(ctx, sess.Token); err != nil {
		t.Fatalf("touch: %v", err)
	}

	key, _, err := s.CreateAPIKey(ctx, u.ID, "ci", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.APIKey(ctx, key); err != nil {
		t.Fatal(err)
	}

	if err := s.SetRole(ctx, u.ID, auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, sess.Token); !errors.Is(err, auth.ErrSessionInvalid) {
		t.Fatal("role change must revoke sessions")
	}
	sess2, _ := s.Login(ctx, "riya@arviawealth.com", pw, ci)
	if err := s.Logout(ctx, sess2.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, sess2.Token); !errors.Is(err, auth.ErrSessionInvalid) {
		t.Fatal("logout must revoke")
	}

	page, err := auditor.Query(ctx, audit.Filter{Entity: "session", Page: paging.Request{Limit: 50}})
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]int{}
	for _, r := range page.Items {
		actions[r.Action]++
	}
	if actions["login"] != 2 || actions["login_failed"] != 2 || actions["logout"] != 1 {
		t.Fatalf("audit actions: %v", actions)
	}
}
