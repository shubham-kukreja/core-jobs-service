package datapoints

import (
	"context"
	"sync"
	"time"

	"cloud.google.com/go/civil"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
)

// Queries in this package are hand-written pgx: bulk writes unnest arrays
// with NULL elements, which sqlc cannot type. All are parameterised.

// sourceCacheTTL bounds how stale the cached source list may be. Sources are
// seeded config and change rarely.
const sourceCacheTTL = time.Minute

// Store implements Writer, Reader, Outbox and RawPayloads on Postgres. It
// resolves series and sources through catalog.Reader and never reads catalog
// tables directly.
type Store struct {
	pool  *pgxpool.Pool
	cat   catalog.Reader
	clock clock.Clock

	mu        sync.Mutex
	sources   []catalog.Source
	sourcesAt time.Time
}

var (
	_ Writer      = (*Store)(nil)
	_ Reader      = (*Store)(nil)
	_ Outbox      = (*Store)(nil)
	_ RawPayloads = (*Store)(nil)
)

// New returns a datapoints store.
func New(pool *pgxpool.Pool, cat catalog.Reader, clk clock.Clock) *Store {
	return &Store{pool: pool, cat: cat, clock: clk}
}

// allSources returns every source, cached for sourceCacheTTL.
func (s *Store) allSources(ctx context.Context) ([]catalog.Source, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sources != nil && s.clock.Now().Sub(s.sourcesAt) < sourceCacheTTL {
		return s.sources, nil
	}
	srcs, err := s.cat.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	s.sources, s.sourcesAt = srcs, s.clock.Now()
	return srcs, nil
}

func (s *Store) sourceKeys(ctx context.Context) (map[int64]string, error) {
	srcs, err := s.allSources(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[int64]string, len(srcs))
	for _, src := range srcs {
		m[src.ID] = src.Key
	}
	return m, nil
}

// dateTime converts an observation date to the time.Time pgx encodes as date.
func dateTime(d civil.Date) time.Time { return d.In(time.UTC) }
