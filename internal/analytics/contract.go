// Package analytics runs backtests: a frozen allocation invested in chosen
// funds, compared with the same cash flows into a benchmark.
package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"cloud.google.com/go/civil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
)

// MaxPending is how many backtests may be queued or running at once.
const MaxPending = 3

// Status is a backtest's lifecycle state.
type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// Source is what allocation the backtest freezes.
type Source struct {
	Kind      string     `json:"kind"` // draft | version
	VersionID *uuid.UUID `json:"id,omitempty"`
}

// Leg is one sleeve invested in one fund.
type Leg struct {
	Sleeve       string          `json:"sleeve"`
	SchemeCode   string          `json:"scheme_code"`
	Amount       decimal.Decimal `json:"amount"`
	PurchaseDate civil.Date      `json:"purchase_date"`
}

// Request submits a backtest.
type Request struct {
	Name         string
	Source       Source
	Benchmark    string
	TotalAmount  decimal.Decimal
	PurchaseDate civil.Date
	Legs         []Leg
}

// Pair holds the allocation and benchmark values of one metric.
type Pair struct {
	Allocation decimal.Decimal `json:"allocation"`
	Benchmark  decimal.Decimal `json:"benchmark"`
	Difference decimal.Decimal `json:"difference"`
}

// Summary is the headline result.
type Summary struct {
	Invested  decimal.Decimal `json:"invested"`
	Current   Pair            `json:"current"`
	AbsReturn Pair            `json:"abs_return"`
	XIRR      Pair            `json:"xirr"`
}

// FundResult is one leg's outcome.
type FundResult struct {
	Sleeve     string          `json:"sleeve"`
	SchemeCode string          `json:"scheme_code"`
	Invested   decimal.Decimal `json:"invested"`
	Current    decimal.Decimal `json:"current"`
	XIRR       decimal.Decimal `json:"xirr"`
}

// Backtest is a stored backtest.
type Backtest struct {
	ID         uuid.UUID
	Name       string
	Status     Status
	Source     Source
	Allocation json.RawMessage
	Benchmark  string
	Legs       []Leg
	Summary    *Summary
	SeriesURI  string
	PerFund    []FundResult
	Warnings   []string
	JobRunID   *int64
	RerunOf    *uuid.UUID
	CreatedBy  int64
	CreatedAt  time.Time
	FinishedAt *time.Time
	Position   int // queue position while queued
}

// Backtests submits and reads backtests. Submit fails with ErrQueueFull when
// MaxPending are already queued or running.
type Backtests interface {
	Submit(ctx context.Context, tx pgx.Tx, r Request, actorID int64) (Backtest, error)
	Rerun(ctx context.Context, tx pgx.Tx, id uuid.UUID, actorID int64) (Backtest, error)
	Get(ctx context.Context, id uuid.UUID) (Backtest, error)
	List(ctx context.Context, p paging.Request) (paging.Page[Backtest], error)
}

// Runner executes one backtest (the backtest.run job).
type Runner interface {
	Run(ctx context.Context, id uuid.UUID, progress func(pct int, msg string)) (Summary, error)
}

var (
	ErrNotFound  = errors.New("analytics: not found")
	ErrQueueFull = errors.New("analytics: backtest queue full")
)
