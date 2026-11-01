// Package market stores fund NAVs and index levels for backtests. Unlike
// datapoints this data is high-volume and not bitemporal.
package market

import (
	"context"
	"errors"

	"cloud.google.com/go/civil"
	"github.com/shopspring/decimal"
)

// Fund is a mutual fund scheme.
type Fund struct {
	SchemeCode string
	Name       string
	AMC        string
	Category   string
	Plan       string
	Active     bool
}

// Level is one dated value (a NAV or an index level).
type Level struct {
	Date  civil.Date
	Value decimal.Decimal
}

// Benchmark is an index a backtest can compare against.
type Benchmark struct {
	Key  string
	Name string
}

// Store reads and writes market data.
type Store interface {
	// SearchFunds matches name or scheme code; q must be at least 3 characters.
	SearchFunds(ctx context.Context, q string, limit int) ([]Fund, error)
	Fund(ctx context.Context, schemeCode string) (Fund, error)
	Benchmarks(ctx context.Context) ([]Benchmark, error)
	// NAVs returns NAVs in [from, to], plus the latest NAV on or before from.
	NAVs(ctx context.Context, schemeCode string, from, to civil.Date) ([]Level, error)
	IndexLevels(ctx context.Context, index string, from, to civil.Date) ([]Level, error)
	UpsertFunds(ctx context.Context, fs []Fund) error
	UpsertNAVs(ctx context.Context, schemeCode string, ls []Level) error
	UpsertIndexLevels(ctx context.Context, index string, ls []Level) error
}

// MinSearchLength is the shortest accepted fund search query.
const MinSearchLength = 3

var ErrNotFound = errors.New("market: not found")
