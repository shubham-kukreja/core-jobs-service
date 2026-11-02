package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2Params are argon2id cost parameters.
type Argon2Params struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
	SaltLen     int
	KeyLen      uint32
}

// DefaultArgon2 exceeds OWASP's argon2id minimums (19 MiB, t=2, p=1).
var DefaultArgon2 = Argon2Params{MemoryKiB: 64 * 1024, Iterations: 3, Parallelism: 2, SaltLen: 16, KeyLen: 32}

// Bounds accepted when parsing stored hashes, so a corrupt row cannot make
// verification consume unbounded memory or time.
const (
	maxMemoryKiB   = 256 * 1024
	maxIterations  = 10
	maxParallelism = 16
)

// Hasher hashes and verifies passwords with argon2id. A semaphore bounds
// concurrent operations so login floods cannot exhaust memory (each one
// allocates MemoryKiB).
type Hasher struct {
	p     Argon2Params
	sem   chan struct{}
	dummy string
}

// NewHasher returns a hasher allowing at most concurrency simultaneous
// operations.
func NewHasher(p Argon2Params, concurrency int) (*Hasher, error) {
	if concurrency < 1 {
		return nil, errors.New("auth: hasher concurrency must be positive")
	}
	if p.MemoryKiB == 0 || p.MemoryKiB > maxMemoryKiB || p.Iterations == 0 || p.Iterations > maxIterations ||
		p.Parallelism == 0 || p.Parallelism > maxParallelism || p.SaltLen < 16 || p.KeyLen < 32 {
		return nil, errors.New("auth: argon2 parameters out of range")
	}
	h := &Hasher{p: p, sem: make(chan struct{}, concurrency)}
	// A real hash of a random password: verifying against it costs as much as
	// verifying a user's hash, so unknown emails take the same time.
	var pw [32]byte
	_, _ = rand.Read(pw[:])
	dummy, err := h.hash(base64.RawStdEncoding.EncodeToString(pw[:]))
	if err != nil {
		return nil, err
	}
	h.dummy = dummy
	return h, nil
}

// Hash returns a PHC-format argon2id hash of password.
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	return h.hash(password)
}

func (h *Hasher) hash(password string) (string, error) {
	salt := make([]byte, h.p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, h.p.Iterations, h.p.MemoryKiB, h.p.Parallelism, h.p.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version,
		h.p.MemoryKiB, h.p.Iterations, h.p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// Verify reports whether password matches the PHC hash, comparing in
// constant time.
func (h *Hasher) Verify(ctx context.Context, password, phc string) (bool, error) {
	p, salt, key, err := parsePHC(phc)
	if err != nil {
		return false, err
	}
	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	defer h.release()
	got := argon2.IDKey([]byte(password), salt, p.Iterations, p.MemoryKiB, p.Parallelism, uint32(len(key))) //nolint:gosec // parsePHC bounds key length to 64.
	return subtle.ConstantTimeCompare(got, key) == 1, nil
}

// Dummy is a valid hash that matches no password, for equal-time failures.
func (h *Hasher) Dummy() string { return h.dummy }

func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hasher) release() { <-h.sem }

func parsePHC(phc string) (Argon2Params, []byte, []byte, error) {
	var p Argon2Params
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return p, nil, nil, errors.New("auth: not an argon2id hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return p, nil, nil, errors.New("auth: unsupported argon2 version")
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Iterations, &p.Parallelism); err != nil {
		return p, nil, nil, errors.New("auth: malformed argon2 parameters")
	}
	if p.MemoryKiB == 0 || p.MemoryKiB > maxMemoryKiB || p.Iterations == 0 || p.Iterations > maxIterations ||
		p.Parallelism == 0 || p.Parallelism > maxParallelism {
		return p, nil, nil, errors.New("auth: argon2 parameters out of range")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return p, nil, nil, errors.New("auth: malformed salt")
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) < 16 || len(key) > 64 {
		return p, nil, nil, errors.New("auth: malformed key")
	}
	return p, salt, key, nil
}
