package api

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
)

const (
	cursorVersion = 1
	cursorMACLen  = sha256.Size
	maxCursorLen  = 512
)

// cursorSigner turns repository keyset positions into opaque, signed cursors
// bound to one endpoint, so clients can neither forge nor transplant them.
type cursorSigner struct {
	key []byte
}

func newCursorSigner(secret []byte) (*cursorSigner, error) {
	if len(secret) < 32 {
		return nil, errors.New("api: secret key must be at least 32 bytes")
	}
	key, err := hkdf.Key(sha256.New, secret, nil, "core/api/cursor/v1", 32)
	if err != nil {
		return nil, err
	}
	return &cursorSigner{key: key}, nil
}

func (c *cursorSigner) mac(scope, pos string) []byte {
	m := hmac.New(sha256.New, c.key)
	m.Write([]byte{cursorVersion})
	m.Write([]byte(scope))
	m.Write([]byte{0})
	m.Write([]byte(pos))
	return m.Sum(nil)
}

// Encode returns a cursor for pos, or nil for the last page.
func (c *cursorSigner) Encode(scope, pos string) *string {
	if pos == "" {
		return nil
	}
	raw := append([]byte{cursorVersion}, pos...)
	raw = append(raw, c.mac(scope, pos)...)
	s := base64.RawURLEncoding.EncodeToString(raw)
	return &s
}

// Decode verifies a cursor and returns its position ("" for none).
func (c *cursorSigner) Decode(scope, cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	bad := apperr.New(apperr.InvalidParams, "Invalid cursor", apperr.Detail{Field: "cursor", Reason: "invalid_cursor"})
	if len(cursor) > maxCursorLen {
		return "", bad
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) < 1+1+cursorMACLen || raw[0] != cursorVersion {
		return "", bad
	}
	pos := string(raw[1 : len(raw)-cursorMACLen])
	if !hmac.Equal(raw[len(raw)-cursorMACLen:], c.mac(scope, pos)) {
		return "", bad
	}
	return pos, nil
}
