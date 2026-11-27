// Package postgres is the PostgreSQL adapter of the audit outbox port.
//
// [NewWriter] makes the audit.Writer. It inserts the entries in the transaction
// of the context, which the adapter transaction/postgres starts. It does not
// write outside a transaction.
//
// The package embeds the migration for the table [Table]. A service runs these
// migrations with their own migrations table [MigrationsTable], before its own
// migrations:
//
//	migrator, err := pgmigrate.NewMigrator(pool, auditpg.Migrations(),
//		pgmigrate.MigratorOptions{MigrationsTable: auditpg.MigrationsTable})
//	if err != nil {
//		return err
//	}
//	_, err = migrate.Run(ctx, pgmigrate.NewLocker(pool, auditpg.MigrationsTable), migrator,
//		migrate.Options{Name: auditpg.MigrationsTable})
//
// The relay reads the table with outbox/postgres.NewStoreFor and [Table].
// Retention uses retention/postgres.NewTablePurger with the time column
// published_at.
package postgres
