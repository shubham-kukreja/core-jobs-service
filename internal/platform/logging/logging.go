// Package logging builds the process logger. Output is JSON; attributes whose
// keys look sensitive are redacted before they are written.
package logging

import (
	"io"
	"log/slog"
	"strings"
)

// sensitiveKeys are matched case-insensitively as substrings of attribute keys.
var sensitiveKeys = []string{
	"password", "secret", "token", "authorization", "cookie", "session",
	"api_key", "apikey", "credential", "private",
}

// New returns a JSON logger at the given level ("debug", "info", "warn", "error").
func New(w io.Writer, level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       lvl,
		ReplaceAttr: redact,
	})
	return slog.New(h)
}

func redact(_ []string, a slog.Attr) slog.Attr {
	k := strings.ToLower(a.Key)
	for _, s := range sensitiveKeys {
		if strings.Contains(k, s) {
			return slog.String(a.Key, "[redacted]")
		}
	}
	return a
}
