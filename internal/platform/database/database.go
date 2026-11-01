// Package database owns the Postgres pool and the transaction helper every
// module uses. Modules never open their own connections.
package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DBTX is satisfied by *pgxpool.Pool and pgx.Tx, so repositories can run
// inside or outside a caller's transaction.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Options tunes the pool.
type Options struct {
	MaxConns         int32
	StatementTimeout time.Duration
	AppName          string
}

// Open connects and verifies the database is reachable.
func Open(ctx context.Context, url string, o Options) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// Never wrap the URL itself: it carries the password.
		return nil, errors.New("database: invalid connection string")
	}
	if o.MaxConns > 0 {
		cfg.MaxConns = o.MaxConns
	}
	rp := cfg.ConnConfig.RuntimeParams
	if o.AppName != "" {
		rp["application_name"] = o.AppName
	}
	if o.StatementTimeout > 0 {
		rp["statement_timeout"] = fmt.Sprint(o.StatementTimeout.Milliseconds())
	}
	// All timestamps are stored and compared in UTC.
	rp["timezone"] = "UTC"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database: connect: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: ping: %w", err)
	}
	return pool, nil
}

// TxBeginner starts transactions; *pgxpool.Pool implements it.
type TxBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// InTx runs fn in a transaction, committing on success and rolling back on
// error or panic. The panic is re-raised after rollback.
func InTx(ctx context.Context, db TxBeginner, fn func(pgx.Tx) error) (err error) {
	return InTxOpts(ctx, db, pgx.TxOptions{}, fn)
}

// InTxOpts is InTx with explicit isolation and access mode.
func InTxOpts(ctx context.Context, db TxBeginner, opts pgx.TxOptions, fn func(pgx.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("database: begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit: %w", err)
	}
	return nil
}

// Postgres error codes the modules map to domain errors.
const (
	CodeUniqueViolation     = "23505"
	CodeForeignKeyViolation = "23503"
	CodeCheckViolation      = "23514"
	CodeSerialization       = "40001"
)

// IsCode reports whether err is a Postgres error with the given SQLSTATE.
func IsCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
