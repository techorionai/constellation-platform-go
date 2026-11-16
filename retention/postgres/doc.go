// Package postgres has the PostgreSQL adapters of the retention ports.
//
// [TablePurger] deletes the rows of one table that are older than a given time,
// in batches. [Locker] is a session advisory lock that does not wait, so that
// only one instance of a service runs a retention job at one time. The adapters
// use the pool directly, because a retention tick is not part of a transaction
// of a caller.
//
//	purger, err := retpg.NewTablePurger(pool, retpg.TableSpec{Table: "audit_entries", TimeColumn: "created_at"})
//	if err != nil {
//		return err
//	}
//	locker, err := retpg.NewLocker(pool)
//	if err != nil {
//		return err
//	}
//
// A tick uses two pool connections at the same time: one for the lock and one
// for each Purge call.
package postgres
