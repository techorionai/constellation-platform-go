// Package migrate runs the database migrations of a service at startup. Only
// one instance runs the migrations at one time. The other instances wait.
//
// The package is a port. [Locker] gives a lock that all instances share.
// [Migrator] applies the pending up migrations. [Run] takes the lock, calls
// [Migrator.Up], and releases the lock. The adapters are in the package
// migrate/postgres. This package does not import a database driver.
//
// A service calls [Run] after it starts the admin server and before it starts
// the API server and the jobs:
//
//	locker := migratepg.NewLocker(db.Pool, "")
//	migrator, err := migratepg.NewMigrator(db.Pool, files, migratepg.MigratorOptions{})
//	if err != nil {
//		return err
//	}
//	_, err = migrate.Run(ctx, locker, migrator, migrate.Options{})
//
// [Run] writes log records, makes the span migrate.run, and records the metric
// migrate.duration. All three have the attribute migrate.name.
package migrate
