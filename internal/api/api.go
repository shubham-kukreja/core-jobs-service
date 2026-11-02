// Package api serves the HTTP API under /v1. Handlers translate HTTP to
// module calls; business rules live in the modules. Every mutation runs in
// one transaction with its audit row.
package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/audit"
	"github.com/shubham-kukreja/core-jobs-service/internal/auth"
	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/ingest"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
)

// Config is the HTTP policy. Build it from platform config.
type Config struct {
	Production      bool
	CORSOrigins     []string       // exact origins; also trusted for cross-origin checks
	TrustedProxies  []netip.Prefix // peers whose X-Forwarded-For is honoured
	MaxRequestBytes int64
	SecretKey       []byte // derives the cursor key
	CookieDomain    string
	// Rate limits. Zero values take the defaults below.
	IPRate      Rate
	UserRate    Rate
	LoginIPRate Rate
	// LoginFailures per email (or IP) within LoginWindow trigger 429.
	LoginFailures int
	LoginWindow   time.Duration
}

// Defaults for rate limits.
var (
	DefaultIPRate      = Rate{Burst: 120, Per: time.Minute}
	DefaultUserRate    = Rate{Burst: 300, Per: time.Minute}
	DefaultLoginIPRate = Rate{Burst: 10, Per: time.Minute}
)

// UserReader resolves users for /v1/me and actor emails; auth.Users satisfies it.
type UserReader interface {
	Get(ctx context.Context, id int64) (auth.User, error)
}

// Rebuilder enqueues derive.cascade in rebuild mode inside the caller's
// transaction after a derivation changes. The lead wires it to the job kind.
type Rebuilder interface {
	EnqueueRebuild(ctx context.Context, tx pgx.Tx, seriesKeys []string, actorID int64) (jobs.RunRef, error)
}

// ConnectorLookup reports which sources have a fetch connector;
// ingest.ConnectorRegistry satisfies it.
type ConnectorLookup interface {
	Get(key string) (ingest.Connector, bool)
}

// Deps are the API's collaborators. Everything except Connectors is required.
type Deps struct {
	DB          database.TxBeginner
	Auth        auth.Authenticator
	Users       UserReader
	Audit       audit.Auditor
	Catalog     catalog.Catalog
	Reader      datapoints.Reader
	Writer      datapoints.Writer
	Rebuilds    Rebuilder
	Idempotency IdempotencyStore
	Connectors  ConnectorLookup
	Clock       clock.Clock
	Log         *slog.Logger
	// Optional overrides (shared stores across instances).
	IPLimiter     Limiter
	UserLimiter   Limiter
	LoginLimiter  Limiter
	LoginFailures FailureTracker
}

// Server holds handler dependencies.
type Server struct {
	cfg           Config
	db            database.TxBeginner
	auth          auth.Authenticator
	users         UserReader
	audit         audit.Auditor
	catalog       catalog.Catalog
	reader        datapoints.Reader
	writer        datapoints.Writer
	rebuilds      Rebuilder
	idem          IdempotencyStore
	connectors    ConnectorLookup
	clock         clock.Clock
	log           *slog.Logger
	cursors       *cursorSigner
	cop           *http.CrossOriginProtection
	ipLimiter     Limiter
	userLimiter   Limiter
	loginLimiter  Limiter
	loginFailures FailureTracker
}

// New returns the /v1 handler (plus /healthz-style probes stay in cmd/app).
func New(cfg Config, d Deps) (http.Handler, error) {
	s, err := newServer(cfg, d)
	if err != nil {
		return nil, err
	}
	return s.routes(), nil
}

func newServer(cfg Config, d Deps) (*Server, error) {
	if d.DB == nil || d.Auth == nil || d.Users == nil || d.Audit == nil || d.Catalog == nil ||
		d.Reader == nil || d.Writer == nil || d.Rebuilds == nil || d.Idempotency == nil || d.Clock == nil {
		return nil, errors.New("api: missing dependency")
	}
	if cfg.MaxRequestBytes <= 0 {
		return nil, errors.New("api: MaxRequestBytes must be positive")
	}
	cursors, err := newCursorSigner(cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	cop := http.NewCrossOriginProtection()
	for _, o := range cfg.CORSOrigins {
		if err := cop.AddTrustedOrigin(o); err != nil {
			return nil, fmt.Errorf("api: origin %q: %w", o, err)
		}
	}
	if d.Log == nil {
		d.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Server{
		cfg: cfg, db: d.DB, auth: d.Auth, users: d.Users, audit: d.Audit, catalog: d.Catalog,
		reader: d.Reader, writer: d.Writer, rebuilds: d.Rebuilds, idem: d.Idempotency, connectors: d.Connectors,
		clock: d.Clock, log: d.Log, cursors: cursors, cop: cop,
		ipLimiter: d.IPLimiter, userLimiter: d.UserLimiter, loginLimiter: d.LoginLimiter, loginFailures: d.LoginFailures,
	}
	rate := func(r, def Rate) Rate {
		if r.Burst <= 0 || r.Per <= 0 {
			return def
		}
		return r
	}
	if s.ipLimiter == nil {
		s.ipLimiter = NewMemLimiter(rate(cfg.IPRate, DefaultIPRate), d.Clock)
	}
	if s.userLimiter == nil {
		s.userLimiter = NewMemLimiter(rate(cfg.UserRate, DefaultUserRate), d.Clock)
	}
	if s.loginLimiter == nil {
		s.loginLimiter = NewMemLimiter(rate(cfg.LoginIPRate, DefaultLoginIPRate), d.Clock)
	}
	if s.loginFailures == nil {
		n, win := cfg.LoginFailures, cfg.LoginWindow
		if n <= 0 {
			n = 5
		}
		if win <= 0 {
			win = 15 * time.Minute
		}
		s.loginFailures = NewMemFailureTracker(n, win, d.Clock)
	}
	return s, nil
}

func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(withRequestID, s.recoverer, s.clientIP, s.accessLog, s.securityHeaders, s.cors, s.crossOrigin, s.limitIP, s.limitBody)
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		s.writeError(w, r, apperr.New(apperr.NotFound, "Not found"))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		s.writeError(w, r, apperr.New(apperr.NotFound, "Not found"))
	})

	r.Route("/v1", func(r chi.Router) {
		r.Post("/auth/login", s.login)

		r.Group(func(r chi.Router) {
			r.Use(s.authenticate)
			v, a, admin := auth.RoleViewer, auth.RoleAnalyst, auth.RoleAdmin

			r.Post("/auth/logout", s.requireRole(v, s.logout))
			r.Get("/me", s.requireRole(v, s.me))

			r.Get("/entities", s.requireRole(v, s.listEntities))
			r.Post("/entities", s.requireRole(admin, s.createEntity))
			r.Patch("/entities/{key}", s.requireRole(admin, s.updateEntity))
			r.Get("/sources", s.requireRole(v, s.listSources))
			r.Get("/series", s.requireRole(v, s.listSeries))
			r.Post("/series", s.requireRole(admin, s.createSeries))
			r.Get("/series/{key}", s.requireRole(v, s.getSeries))
			r.Patch("/series/{key}", s.requireRole(admin, s.updateSeries))
			r.Get("/series/{key}/deps", s.requireRole(v, s.seriesDeps))

			r.Post("/datapoints", s.requireRole(a, s.writeDatapoints))
			r.Get("/series/{key}/datapoints", s.requireRole(v, s.listSeriesDatapoints))
			r.Get("/series/{key}/datapoints/{obs_date}", s.requireRole(v, s.getSeriesDatapoint))
			r.Get("/series/{key}/datapoints/{obs_date}/versions", s.requireRole(v, s.datapointVersions))
			r.Get("/datapoints/{id}/lineage", s.requireRole(v, s.datapointLineage))
			r.Post("/datapoints/{id}/retract", s.requireRole(a, s.retractDatapoint))
			r.Post("/snapshot", s.requireRole(v, s.snapshot))
		})
	})
	return r
}
