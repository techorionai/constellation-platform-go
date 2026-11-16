// Package postgres is the PostgreSQL adapter of the transaction port in the
// package transaction.
//
// [Transactor] starts a transaction on a pgxpool pool and puts it in the
// context with clients/postgres.ContextWithTx. A repository adapter gets its
// connection with clients/postgres.Conn(ctx, pool), so it joins the
// transaction of the caller. Only this adapter calls ContextWithTx.
//
// A nested WithinTx call joins the outer transaction. The AfterCommit hooks run
// in the order of registration after the commit of the outermost transaction.
// Each hook has a time limit ([WithHookTimeout]). The adapter makes the span
// db.transaction and records the metrics constellation.db.transaction.*.
//
//	tx := txpg.NewTransactor(db.Pool)
//	err := tx.WithinTx(ctx, func(ctx context.Context) error {
//		_, err := postgres.Conn(ctx, db.Pool).Exec(ctx, "INSERT INTO items (id) VALUES ($1)", id)
//		return err
//	}, transaction.Name("create_item"))
package postgres
