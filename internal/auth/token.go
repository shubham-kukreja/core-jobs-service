package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
)

// tokenBytes is the entropy of session, CSRF and API-key secrets (256 bits).
const tokenBytes = 32

// apiKeyPrefix starts every API key so leaked keys are easy to recognise and scan for.
const apiKeyPrefix = "core_"

var (
	// tokenRE matches a base64url-encoded 32-byte token (43 characters).
	tokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	// apiKeyRE matches core_<8 hex prefix>_<43 char secret>.
	apiKeyRE = regexp.MustCompile(`^core_([0-9a-f]{8})_[A-Za-z0-9_-]{43}$`)
)

// newToken returns a random URL-safe token.
func newToken() (string, error) {
	var b [tokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("auth: random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// newAPIKey returns a full API key and its display prefix.
func newAPIKey() (key, prefix string, err error) {
	var p [4]byte
	if _, err := rand.Read(p[:]); err != nil {
		return "", "", fmt.Errorf("auth: random prefix: %w", err)
	}
	secret, err := newToken()
	if err != nil {
		return "", "", err
	}
	prefix = hex.EncodeToString(p[:])
	return apiKeyPrefix + prefix + "_" + secret, prefix, nil
}

// HashToken returns the SHA-256 of a token: what is stored and compared.
// Tokens carry 256 bits of entropy, so a fast hash is sufficient.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// TokenMatches reports, in constant time, whether token hashes to want.
func TokenMatches(token string, want []byte) bool {
	return len(want) == sha256.Size && subtle.ConstantTimeCompare(HashToken(token), want) == 1
}

// validToken reports whether s has the shape of a session or CSRF token.
func validToken(s string) bool { return tokenRE.MatchString(s) }
