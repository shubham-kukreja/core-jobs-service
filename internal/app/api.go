package app

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"runtime"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shubham-kukreja/core-jobs-service/internal/api"
	"github.com/shubham-kukreja/core-jobs-service/internal/audit"
	"github.com/shubham-kukreja/core-jobs-service/internal/auth"
	"github.com/shubham-kukreja/core-jobs-service/internal/derive"
	"github.com/shubham-kukreja/core-jobs-service/internal/ingest"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/config"
)

// NewAuth builds the auditor and the auth service. The admin CLI uses it
// without the rest of the core.
func NewAuth(pool *pgxpool.Pool, cfg config.Config, clk clock.Clock) (*auth.Service, *audit.PG, error) {
	auditor := audit.NewPG(pool)
	hasher, err := auth.NewHasher(auth.DefaultArgon2, max(2, runtime.NumCPU()/2))
	if err != nil {
		return nil, nil, err
	}
	acfg := auth.DefaultConfig()
	acfg.SessionIdleTTL, acfg.SessionAbsoluteTTL = cfg.SessionIdleTTL, cfg.SessionAbsoluteTTL
	svc, err := auth.NewService(auth.NewPGStore(pool, auditor), hasher, auth.NewPolicy(), clk, acfg)
	if err != nil {
		return nil, nil, err
	}
	return svc, auditor, nil
}

// NewAPI builds the /v1 handler.
func (c *Core) NewAPI(cfg config.Config) (http.Handler, error) {
	authSvc, auditor, err := NewAuth(c.Pool, cfg, c.Clock)
	if err != nil {
		return nil, err
	}
	proxies, err := parsePrefixes(cfg.TrustedProxyCIDRs)
	if err != nil {
		return nil, err
	}
	return api.New(api.Config{
		Production:      cfg.IsProduction(),
		CORSOrigins:     cfg.CORSOrigins,
		TrustedProxies:  proxies,
		MaxRequestBytes: cfg.MaxRequestBytes,
		MaxUploadBytes:  cfg.MaxUploadBytes,
		SecretKey:       []byte(cfg.SecretKey.Reveal()),
		CookieDomain:    cfg.CookieDomain,
	}, api.Deps{
		DB:          c.Pool,
		Auth:        authSvc,
		Users:       authSvc,
		Audit:       auditor,
		Catalog:     c.Catalog,
		Reader:      c.Datapoints,
		Writer:      c.Datapoints,
		Rebuilds:    rebuilds{c.Jobs},
		Idempotency: api.NewPGIdempotency(c.Pool),
		Connectors:  noConnectors{},
		Jobs: api.JobDeps{
			Jobs: c.Jobs, Runs: c.Runs, Schedules: c.Schedules, Kinds: c.Registry,
			Uploads: c.Uploads, Mappings: c.Mappings,
		},
		Clock: c.Clock,
		Log:   c.Log,
	})
}

func parsePrefixes(cidrs []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, s := range cidrs {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXY_CIDRS: %q: %w", s, err)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// rebuilds enqueues derive.cascade in rebuild mode, with everything
// downstream, in the caller's transaction after a derivation changes.
type rebuilds struct{ jobs jobs.Enqueuer }

func (r rebuilds) EnqueueRebuild(ctx context.Context, tx pgx.Tx, keys []string, actorID int64) (jobs.RunRef, error) {
	return r.jobs.EnqueueTx(ctx, tx, derive.CascadeArgs{Mode: derive.ModeRebuild, Series: keys, IncludeDownstream: true},
		jobs.WithTrigger(jobs.TriggerUser), jobs.WithUser(actorID))
}

// noConnectors reports that no source has a fetch connector yet (Phase 5).
type noConnectors struct{}

func (noConnectors) Get(string) (ingest.Connector, bool) { return nil, false }
