/*
@Author: Franco Ribeiro Borba
@Description: PostgreSQL connection pool. It is created once and shared by
every repository, worker and health check, because a pool is what turns a fixed
number of connections into concurrency: the consumer handling several messages
at the same time and the publisher claiming a batch all draw from it. The pool
is verified with a ping at startup, so a wrong URL or an unreachable database
stops the process there instead of failing later inside a financial operation,
and Ping is exposed for the readiness probe, which must answer on the state of
the dependencies and not only on the process being alive.
@Date : 20/09/2026
@Update: -
*/
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool opens the pool and checks that the database answers.
func NewPool(ctx context.Context, cfg config.Database) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	poolConfig.MaxConns = cfg.MaxConnections
	poolConfig.MaxConnLifetime = time.Hour
	poolConfig.MaxConnIdleTime = 30 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()

	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("reach database: %w", err)
	}

	return pool, nil
}
