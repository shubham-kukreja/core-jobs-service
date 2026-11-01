package logging

import (
	"bytes"
	"strings"
	"testing"
)

func TestRedactsSensitiveKeys(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, "info")
	log.Info("login", "email", "a@b.c", "password", "hunter2", "session_id", "abc", "Authorization", "Bearer x")
	out := buf.String()
	for _, leaked := range []string{"hunter2", "\"abc\"", "Bearer x"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("leaked %q in %s", leaked, out)
		}
	}
	if !strings.Contains(out, "a@b.c") {
		t.Fatalf("non-sensitive attr dropped: %s", out)
	}
}
