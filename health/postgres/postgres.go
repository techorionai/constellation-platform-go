package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/techorionai/constellation-platform-go/health"
)

var _ health.Checker = (*Checker)(nil)

// Checker checks PostgreSQL with Ping on its own pool.
type Checker struct {
	pool *pgxpool.Pool
}

// NewChecker copies the config of the service pool, sets MaxConns to 1, MinConns
// to 0, and the tracer to nil, and makes a dedicated pool. Call
// [Checker.Close] at shutdown.
func NewChecker(ctx context.Context, pool *pgxpool.Pool) (*Checker, error) {
	if pool == nil {
		return nil, errors.New("health: pool is required")
	}
	cfg := pool.Config()
	cfg.MaxConns = 1
	cfg.MinConns = 0
	cfg.ConnConfig.Tracer = nil
	dedicated, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("health: cannot create the checker pool: %w", err)
	}
	return &Checker{pool: dedicated}, nil
}

// Name returns "postgres".
func (c *Checker) Name() string { return "postgres" }

// Check calls Ping on the dedicated pool.
func (c *Checker) Check(ctx context.Context) error { return c.pool.Ping(ctx) }

// Close closes the dedicated pool. The service pool stays open.
func (c *Checker) Close() { c.pool.Close() }
