package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
)

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyClientIP
	keyPrincipal
)

func requestIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(keyRequestID).(string)
	return s
}

func clientIPFrom(ctx context.Context) netip.Addr {
	a, _ := ctx.Value(keyClientIP).(netip.Addr)
	return a
}

// inboundRequestID accepts only conservative IDs from upstream proxies, so a
// client cannot inject log-forging content.
var inboundRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "req_" + hex.EncodeToString(b[:])
}

// withRequestID assigns every request an id, echoed in X-Request-Id and in
// error bodies.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !inboundRequestID.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), keyRequestID, id)))
	})
}

// recoverer turns panics into a generic 500; the stack goes to the log only.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does.
					panic(v)
				}
				s.log.ErrorContext(r.Context(), "panic", "request_id", requestIDFrom(r.Context()), "panic", v)
				s.writeError(w, r, apperr.New(apperr.Internal, "Internal error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// clientIP resolves the caller's address. X-Forwarded-For is honoured only
// when the direct peer is a trusted proxy; the rightmost untrusted hop wins.
func (s *Server) clientIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := peerIP(r.RemoteAddr)
		if s.trusted(ip) {
			hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
			for i := len(hops) - 1; i >= 0; i-- {
				h, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
				if err != nil {
					break
				}
				ip = h.Unmap()
				if !s.trusted(ip) {
					break
				}
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), keyClientIP, ip)))
	})
}

func peerIP(remote string) netip.Addr {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

func (s *Server) trusted(ip netip.Addr) bool {
	for _, p := range s.cfg.TrustedProxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// securityHeaders applies the response headers from docs/security.md.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		if s.cfg.Production || r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// cors allows credentialed requests from the exact configured origins only.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := origin != "" && s.allowedOrigin(origin)
		if origin != "" {
			w.Header().Add("Vary", "Origin")
		}
		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Expose-Headers", "X-Request-Id, Retry-After")
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			if !allowed {
				s.writeError(w, r, apperr.New(apperr.Forbidden, "Origin not allowed"))
				return
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-CSRF-Token, Idempotency-Key, Authorization")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) allowedOrigin(o string) bool {
	for _, a := range s.cfg.CORSOrigins {
		if o == a {
			return true
		}
	}
	return false
}

// crossOrigin rejects cross-site browser requests with unsafe methods
// (Sec-Fetch-Site / Origin vs Host), the first CSRF layer. Cookie-authenticated
// requests must also pass the per-session token check.
func (s *Server) crossOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.cop.Check(r); err != nil {
			s.writeError(w, r, apperr.New(apperr.Forbidden, "Cross-origin request rejected"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limitBody caps request bodies.
func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > s.cfg.MaxRequestBytes {
			s.writeError(w, r, apperr.New(apperr.PayloadTooLarge, "Request body too large"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes)
		next.ServeHTTP(w, r)
	})
}

// limitIP applies the per-IP request budget before authentication.
func (s *Server) limitIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, retry := s.ipLimiter.Allow("ip:" + clientIPFrom(r.Context()).String()); !ok {
			s.rateLimited(w, r, retry)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) rateLimited(w http.ResponseWriter, r *http.Request, retry time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int((retry+time.Second-1)/time.Second)))
	s.writeError(w, r, apperr.New(apperr.RateLimited, "Too many requests; try again later"))
}

// statusRecorder captures the status for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// accessLog writes one line per request. It never logs query strings,
// headers or bodies, which may carry personal data or secrets.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.clock.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		attrs := []any{
			"request_id", requestIDFrom(r.Context()), "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration_ms", s.clock.Now().Sub(start).Milliseconds(),
		}
		if p, ok := principalFrom(r.Context()); ok {
			attrs = append(attrs, "user_id", p.UserID, "auth", string(p.Kind))
		}
		s.log.InfoContext(r.Context(), "http request", attrs...)
	})
}
