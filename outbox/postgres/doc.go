// Package postgres is the PostgreSQL adapter of the outbox ports.
//
// [NewWriter] makes the outbox.Writer. It inserts the events in the transaction
// of the context, which the adapter transaction/postgres starts. It does not
// write outside a transaction. [NewStore] makes the outbox.Store for the relay.
// It locks unpublished rows with FOR UPDATE SKIP LOCKED, so more than one
// relay can run.
//
// The package embeds the migration for the table [Table]. A service runs these
// migrations with their own migrations table [MigrationsTable], before its own
// migrations:
//
//	migrator, err := pgmigrate.NewMigrator(pool, outboxpg.Migrations(),
//		pgmigrate.MigratorOptions{MigrationsTable: outboxpg.MigrationsTable})
//	if err != nil {
//		return err
//	}
//	_, err = migrate.Run(ctx, pgmigrate.NewLocker(pool, outboxpg.MigrationsTable), migrator,
//		migrate.Options{Name: outboxpg.MigrationsTable})
//
// Retention uses retention/postgres.NewTablePurger with the time column
// published_at. A row with no published_at stays.
package postgres
