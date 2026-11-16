package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is the common part of *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

type txKey struct{}

// ContextWithTx returns a context that carries tx. Only the transaction adapter
// (the package transaction/postgres) calls it. A context with a transaction
// that the adapter did not start makes the adapter refuse the context.
func ContextWithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

// TxFromContext returns the transaction in ctx.
func TxFromContext(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	return tx, ok
}

// Conn returns the transaction in ctx, or pool if ctx has no transaction. A
// repository adapter gets its connection with Conn(ctx, pool) for each call, so
// that it joins the transaction of the caller.
func Conn(ctx context.Context, pool *pgxpool.Pool) Querier {
	if tx, ok := TxFromContext(ctx); ok {
		return tx
	}
	return pool
}
