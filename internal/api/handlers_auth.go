package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/shubham-kukreja/core-jobs-service/internal/auth"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
)

type userJSON struct {
	ID    uuid.UUID `json:"id"`
	Email string    `json:"email"`
	Name  string    `json:"name"`
	Role  auth.Role `json:"role"`
}

func toUserJSON(u auth.User) userJSON {
	return userJSON{ID: u.UUID, Email: u.Email, Name: u.Name, Role: u.Role}
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// invalidLogin is the single response for every credential failure.
var invalidLogin = apperr.New(apperr.Unauthorized, "Invalid email or password")

// login rate-limits per IP and counts failures per email and per IP whether
// or not the account exists, so 429 never reveals which emails are registered.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIPFrom(r.Context())
	if ok, retry := s.loginLimiter.Allow("login:" + ip.String()); !ok {
		s.rateLimited(w, r, retry)
		return
	}
	var req loginRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if len(email) < 3 || len(email) > 254 || req.Password == "" || len(req.Password) > 1024 {
		s.writeError(w, r, apperr.New(apperr.ValidationFailed, "Email and password are required"))
		return
	}
	// Keyed by a hash so the tracker never holds raw addresses.
	sum := sha256.Sum256([]byte(email))
	emailKey, ipKey := "email:"+hex.EncodeToString(sum[:]), "ip:"+ip.String()
	for _, k := range []string{emailKey, ipKey} {
		if blocked, retry := s.loginFailures.Blocked(k); blocked {
			s.rateLimited(w, r, retry)
			return
		}
	}

	// Rotate: a session already on this browser is revoked before a new one is issued.
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.auth.Logout(r.Context(), c.Value)
	}

	sess, err := s.auth.Login(r.Context(), email, req.Password, auth.ClientInfo{IP: ip, UserAgent: r.UserAgent()})
	if err != nil {
		if !errors.Is(err, auth.ErrInvalidCredentials) && !errors.Is(err, auth.ErrLocked) {
			s.writeError(w, r, err)
			return
		}
		s.loginFailures.Fail(emailKey)
		s.loginFailures.Fail(ipKey)
		// A locked account answers like any other failure until the email's
		// failure budget is spent, so lockout cannot be used to probe accounts.
		if blocked, retry := s.loginFailures.Blocked(emailKey); blocked {
			s.rateLimited(w, r, retry)
			return
		}
		s.writeError(w, r, invalidLogin)
		return
	}
	s.loginFailures.Reset(emailKey)
	s.setSessionCookies(w, sess.Token, sess.CSRFToken, sess.ExpiresAt)
	writeJSON(w, http.StatusOK, map[string]any{"user": toUserJSON(sess.User)})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r.Context())
	if p.Kind != auth.PrincipalSession {
		s.writeError(w, r, apperr.New(apperr.Forbidden, "Logout applies to browser sessions; revoke API keys instead"))
		return
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		s.writeError(w, r, apperr.New(apperr.Unauthorized, "Authentication required"))
		return
	}
	if err := s.auth.Logout(r.Context(), c.Value); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.clearSessionCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r.Context())
	u, err := s.users.Get(r.Context(), p.UserID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": toUserJSON(u)})
}

func (s *Server) setSessionCookies(w http.ResponseWriter, token, csrf string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", Domain: s.cfg.CookieDomain, Expires: expires,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	// Readable by the frontend, which echoes it in X-CSRF-Token; it is useless
	// without the HttpOnly session cookie, which scripts cannot read.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: HttpOnly must be off so the frontend can read the CSRF token.
		Name: csrfCookie, Value: csrf, Path: "/", Domain: s.cfg.CookieDomain, Expires: expires,
		HttpOnly: false, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) clearSessionCookies(w http.ResponseWriter) {
	for _, name := range []string{sessionCookie, csrfCookie} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/", Domain: s.cfg.CookieDomain, MaxAge: -1,
			HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
		})
	}
}
