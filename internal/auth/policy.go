package auth

import (
	"bufio"
	_ "embed"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Password length bounds, in characters. The upper bound stops very long
// inputs being used to burn hashing time.
const (
	MinPasswordLen = 12
	MaxPasswordLen = 128
)

//go:embed common_passwords.txt
var commonPasswordsTxt string

// Policy implements PasswordPolicy: length bounds, a small embedded list of
// common passwords, and no password containing the email.
type Policy struct {
	common map[string]struct{}
}

var _ PasswordPolicy = (*Policy)(nil)

// NewPolicy returns the default policy.
func NewPolicy() *Policy {
	p := &Policy{common: map[string]struct{}{}}
	sc := bufio.NewScanner(strings.NewReader(commonPasswordsTxt))
	for sc.Scan() {
		if w := strings.TrimSpace(sc.Text()); w != "" && !strings.HasPrefix(w, "#") {
			p.common[strings.ToLower(w)] = struct{}{}
		}
	}
	return p
}

// Check returns ErrWeakPassword wrapped with the reason. The password itself
// never appears in the error.
func (p *Policy) Check(password, email string) error {
	if !utf8.ValidString(password) {
		return fmt.Errorf("%w: not valid UTF-8", ErrWeakPassword)
	}
	n := utf8.RuneCountInString(password)
	switch {
	case n < MinPasswordLen:
		return fmt.Errorf("%w: at least %d characters", ErrWeakPassword, MinPasswordLen)
	case n > MaxPasswordLen:
		return fmt.Errorf("%w: at most %d characters", ErrWeakPassword, MaxPasswordLen)
	}
	lower := strings.ToLower(password)
	if _, ok := p.common[lower]; ok {
		return fmt.Errorf("%w: too common", ErrWeakPassword)
	}
	first, _ := utf8.DecodeRuneInString(lower)
	if strings.Count(lower, string(first)) == n {
		return fmt.Errorf("%w: a single repeated character", ErrWeakPassword)
	}
	e := strings.ToLower(strings.TrimSpace(email))
	local, _, _ := strings.Cut(e, "@")
	if lower == e || (len(local) >= 4 && strings.Contains(lower, local)) {
		return fmt.Errorf("%w: must not contain the email", ErrWeakPassword)
	}
	return nil
}
