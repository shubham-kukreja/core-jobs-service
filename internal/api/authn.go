package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/shubham-kukreja/core-jobs-service/internal/audit"
	"github.com/shubham-kukreja/core-jobs-service/internal/auth"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
)

// Cookie names (see api/openapi.yaml securitySchemes).
const (
	sessionCookie = "core_session"
	csrfCookie    = "core_csrf"
	csrfHeader    = "X-CSRF-Token"
)

func principalFrom(ctx context.Context) (auth.Principal, bool) {
	p, ok := ctx.Value(keyPrincipal).(auth.Principal)
	return p, ok
}

// mustPrincipal returns the authenticated caller. Routes behind authenticate
// always have one.
func mustPrincipal(ctx context.Context) auth.Principal {
	p, ok := principalFrom(ctx)
	if !ok {
		panic("api: handler reached without a principal")
	}
	return p
}

func unsafeMethod(m string) bool {
	return m != http.MethodGet && m != http.MethodHead && m != http.MethodOptions
}

// authenticate resolves the caller from exactly one credential: the session
// cookie or a bearer API key. Presenting both is rejected rather than guessed.
// Cookie-authenticated unsafe requests must carry the session's CSRF token.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, _ := r.Cookie(sessionCookie)
		authz := r.Header.Get("Authorization")
		unauthorized := apperr.New(apperr.Unauthorized, "Authentication required")

		var p auth.Principal
		var err error
		switch {
		case cookie != nil && authz != "":
			s.writeError(w, r, apperr.New(apperr.Unauthorized, "Use either a session cookie or an API key, not both"))
			return
		case authz != "":
			scheme, key, ok := strings.Cut(authz, " ")
			if !ok || !strings.EqualFold(scheme, "Bearer") || key == "" {
				s.writeError(w, r, unauthorized)
				return
			}
			p, err = s.auth.APIKey(r.Context(), strings.TrimSpace(key))
		case cookie != nil:
			p, err = s.auth.Session(r.Context(), cookie.Value)
		default:
			s.writeError(w, r, unauthorized)
			return
		}
		if err != nil {
			if errors.Is(err, auth.ErrSessionInvalid) {
				s.writeError(w, r, unauthorized)
			} else {
				s.writeError(w, r, err)
			}
			return
		}
		if p.Kind == auth.PrincipalSession && unsafeMethod(r.Method) {
			if !auth.TokenMatches(r.Header.Get(csrfHeader), p.CSRFHash) {
				s.writeError(w, r, apperr.New(apperr.Forbidden, "CSRF token missing or invalid"))
				return
			}
		}
		if ok, retry := s.userLimiter.Allow("user:" + p.UserUUID.String()); !ok {
			s.rateLimited(w, r, retry)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), keyPrincipal, p)))
	})
}

// requireRole wraps h so callers below min get 403.
func (s *Server) requireRole(min auth.Role, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := mustPrincipal(r.Context())
		if !p.Role.AtLeast(min) {
			s.writeError(w, r, apperr.New(apperr.Forbidden, "Requires role "+string(min)))
			return
		}
		h(w, r)
	}
}

// auditEntry builds an audit entry for the current request's actor.
func auditEntry(r *http.Request, entity, id, action string, before, after any) audit.Entry {
	e := audit.Entry{
		RequestID: requestIDFrom(r.Context()), IP: clientIPFrom(r.Context()),
		Entity: entity, EntityID: id, Action: action, Before: before, After: after, ActorKind: audit.ActorSystem,
	}
	if p, ok := principalFrom(r.Context()); ok {
		uid := p.UserID
		e.ActorID = &uid
		e.ActorKind = audit.ActorUser
		if p.Kind == auth.PrincipalAPIKey {
			e.ActorKind = audit.ActorAPIKey
		}
	}
	return e
}
