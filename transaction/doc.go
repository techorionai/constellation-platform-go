// Package transaction holds the transaction port.
//
// [Transactor.WithinTx] runs a function in one transaction. It commits if the
// function returns nil, and it rolls back if the function returns an error or
// panics. [Transactor.AfterCommit] registers a function that runs after the
// commit of the outermost transaction, for example to delete a cache entry. The
// adapter for PostgreSQL is in the package transaction/postgres. This package
// does not import a database driver.
//
//	err := tx.WithinTx(ctx, func(ctx context.Context) error {
//		if err := repo.Save(ctx, order); err != nil {
//			return err
//		}
//		tx.AfterCommit(ctx, func(ctx context.Context) { _ = orders.Delete(ctx, order.ID) })
//		return nil
//	}, transaction.Name("create_order"))
//
// A nested WithinTx call joins the outer transaction. It can set [Name] only.
// There are no savepoints. [Resolve] checks the options, so that an adapter and
// a mock check them in the same way.
package transaction
