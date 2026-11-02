package audit

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type inner struct {
	Label    string `json:"label"`
	APIKey   string `json:"api_key"`
	Internal string `audit:"-"`
}

type sample struct {
	Email        string            `json:"email"`
	PasswordHash string            `json:"password_hash"`
	Token        string            `json:"-"`
	Nested       inner             `json:"nested"`
	Attrs        map[string]string `json:"attrs"`
	At           time.Time         `json:"at"`
	ID           uuid.UUID         `json:"id"`
	Raw          []byte            `json:"raw"`
	Empty        string            `json:"empty,omitempty"`
	List         []inner           `json:"list"`
}

func TestSnapshotRedacts(t *testing.T) {
	v := sample{
		Email:        "a@b.co",
		PasswordHash: "$argon2id$secret",
		Token:        "tok-secret",
		Nested:       inner{Label: "ok", APIKey: "key-secret", Internal: "internal-secret"},
		Attrs:        map[string]string{"session_id": "sess-secret", "colour": "blue", "Authorization": "Bearer x-secret"},
		At:           time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		ID:           uuid.MustParse("0b8f2d4e-1c3a-4f5b-9d6e-7a8b9c0d1e2f"),
		Raw:          []byte("hash-bytes-secret"),
		List:         []inner{{Label: "l", APIKey: "list-secret"}},
	}
	b, err := Snapshot(v)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "secret") {
		t.Fatalf("secret leaked: %s", s)
	}
	for _, want := range []string{`"email":"a@b.co"`, `"colour":"blue"`, `"label":"ok"`, `"at":"2026-11-01T00:00:00Z"`, `"id":"0b8f2d4e-1c3a-4f5b-9d6e-7a8b9c0d1e2f"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
	if strings.Contains(s, "empty") {
		t.Errorf("omitempty field kept: %s", s)
	}
}

func TestSnapshotShapes(t *testing.T) {
	if b, _ := Snapshot(nil); b != nil {
		t.Fatalf("nil: %s", b)
	}
	b, _ := Snapshot("plain")
	if string(b) != `{"value":"plain"}` {
		t.Fatalf("scalar wrap: %s", b)
	}
	b, _ = Snapshot(json.RawMessage(`{"token":"x","keep":1}`))
	if string(b) != `{"keep":1}` {
		t.Fatalf("raw message: %s", b)
	}
	var p *sample
	if b, _ := Snapshot(p); b != nil {
		t.Fatalf("nil pointer: %s", b)
	}
}
