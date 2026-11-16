// Package postgres is the PostgreSQL adapter of the secrets.Repository port.
//
// [Repository] stores the envelopes in the table platform_secrets. It gets its
// connection with clients/postgres.Conn for each call, so that
// secrets.Service.Put joins the transaction of the caller. The package embeds
// the migration for the table. A service runs these migrations with its own
// migrations table [MigrationsTable]:
//
//	migrator, err := pgmigrate.NewMigrator(pool, secretspg.Migrations(),
//		pgmigrate.MigratorOptions{MigrationsTable: secretspg.MigrationsTable})
//	if err != nil {
//		return err
//	}
//	_, err = migrate.Run(ctx, pgmigrate.NewLocker(pool, secretspg.MigrationsTable), migrator,
//		migrate.Options{Name: secretspg.MigrationsTable})
//
// The table holds only ciphertext. This package never handles a plaintext value
// or a master key.
package postgres
