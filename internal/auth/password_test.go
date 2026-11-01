package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// testArgon2 keeps unit tests fast; production uses DefaultArgon2.
var testArgon2 = Argon2Params{MemoryKiB: 1024, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32}

func TestHasherRoundTrip(t *testing.T) {
	h, err := NewHasher(testArgon2, 2)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	phc, err := h.Hash(ctx, "a long enough passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=1024,t=1,p=1$") {
		t.Fatalf("unexpected PHC: %s", phc)
	}
	if ok, err := h.Verify(ctx, "a long enough passphrase", phc); err != nil || !ok {
		t.Fatalf("verify correct: %v %v", ok, err)
	}
	if ok, _ := h.Verify(ctx, "wrong passphrase!!", phc); ok {
		t.Fatal("verify wrong password succeeded")
	}
	if ok, _ := h.Verify(ctx, "anything at all", h.Dummy()); ok {
		t.Fatal("dummy hash matched")
	}
}

func TestDefaultParamsAreOWASPGrade(t *testing.T) {
	if DefaultArgon2.MemoryKiB < 19*1024 || DefaultArgon2.Iterations < 2 {
		t.Fatalf("defaults below OWASP minimum: %+v", DefaultArgon2)
	}
	if _, err := NewHasher(DefaultArgon2, 1); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRejectsHostileHashes(t *testing.T) {
	h, _ := NewHasher(testArgon2, 1)
	for _, phc := range []string{
		"plaintext",
		"$argon2i$v=19$m=1024,t=1,p=1$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=99999999,t=1,p=1$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=1024,t=1000,p=1$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5a2V5",
	} {
		if _, err := h.Verify(context.Background(), "x", phc); err == nil {
			t.Errorf("accepted %q", phc)
		}
	}
}

func TestHasherBoundsConcurrency(t *testing.T) {
	h, _ := NewHasher(testArgon2, 1)
	h.sem <- struct{}{} // occupy the only slot
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := h.Hash(ctx, "a long enough passphrase"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded while saturated, got %v", err)
	}
	<-h.sem
}

func TestPolicy(t *testing.T) {
	p := NewPolicy()
	bad := map[string]string{
		"short":         "short1!",
		"too long":      strings.Repeat("ab", 65),
		"common":        "Password1234",
		"repeated":      "zzzzzzzzzzzzzz",
		"email":         "riya@arviawealth.com",
		"email local":   "my-riya.kapoor-2026",
		"invalid utf-8": "abcdefghijk\xff",
	}
	for name, pw := range bad {
		t.Run(name, func(t *testing.T) {
			err := p.Check(pw, "riya.kapoor@arviawealth.com")
			if name == "email" {
				err = p.Check(pw, "riya@arviawealth.com")
			}
			if !errors.Is(err, ErrWeakPassword) {
				t.Fatalf("want ErrWeakPassword, got %v", err)
			}
			if strings.Contains(err.Error(), pw) {
				t.Fatal("error leaks the password")
			}
		})
	}
	if err := p.Check("tidal-saffron-lantern-41", "riya@arviawealth.com"); err != nil {
		t.Fatalf("good password rejected: %v", err)
	}
}

func TestTokens(t *testing.T) {
	a, _ := newToken()
	b, _ := newToken()
	if a == b || !validToken(a) {
		t.Fatalf("tokens: %q %q", a, b)
	}
	if !TokenMatches(a, HashToken(a)) || TokenMatches(b, HashToken(a)) || TokenMatches(a, []byte("short")) {
		t.Fatal("TokenMatches")
	}
	key, prefix, _ := newAPIKey()
	m := apiKeyRE.FindStringSubmatch(key)
	if m == nil || m[1] != prefix {
		t.Fatalf("api key shape: %q", key)
	}
}
