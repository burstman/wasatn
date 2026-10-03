// Package db owns the Postgres connection pool and hands out sqlc queries.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/burstman/wasatn/internal/config"
	"github.com/burstman/wasatn/internal/db/sqlc"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect opens a pool and verifies it can reach the database.
//
// PgBouncer in transaction mode is supported because every query here is
// self-contained; nothing relies on session-scoped state.
func Connect(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("db: parse DATABASE_URL: %w", err)
	}

	poolCfg.MaxConns = 20
	poolCfg.MinConns = 2
	poolCfg.MaxConnLifetime = time.Hour
	poolCfg.MaxConnIdleTime = 30 * time.Minute
	poolCfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// NewQueries returns the generated query set bound to a pool.
func NewQueries(pool *pgxpool.Pool) *sqlc.Queries {
	return sqlc.New(pool)
}

// WithTx runs fn inside a transaction, committing on success and rolling back
// on error or panic.
//
// Transactional enqueue matters here: campaign send jobs are inserted in the
// same transaction that creates the campaign's message log rows, so a crash
// cannot leave messages queued for a campaign that was never created.
func WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(q *sqlc.Queries) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}
	defer func() {
		// Rollback is a no-op once the transaction is committed.
		_ = tx.Rollback(ctx)
	}()

	if err := fn(sqlc.New(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}
	return nil
}
