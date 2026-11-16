// Package postgres is the PostgreSQL adapter of the package migrate.
//
// [Locker] is a session advisory lock on one pooled connection. It waits as long
// as the context allows, so a long migration of another instance does not make
// a waiting instance fail. [Migrator] runs golang-migrate on one pool
// connection, inside the lock. The migrator uses the pool of the service, so the
// migrations run in the database that clients/postgres checked, and the
// migrator never gets a connection string.
//
// Use the same name for the locker, for MigratorOptions.MigrationsTable, and for
// migrate.Options.Name:
//
//	locker := postgres.NewLocker(pool, "schema_migrations")
//	migrator, err := postgres.NewMigrator(pool, files, postgres.MigratorOptions{
//		MigrationsTable: "schema_migrations",
//	})
//
// The pool must have MaxConns of 2 or more, because migrate.Run uses two
// connections at the same time.
package postgres
