package postgres_test

import (
	"context"
	"fmt"
	"io/fs"
	"log"

	clientpg "github.com/techorionai/constellation-platform-go/clients/postgres"
	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/migrate"
	pgmigrate "github.com/techorionai/constellation-platform-go/migrate/postgres"
	"github.com/techorionai/constellation-platform-go/secrets"
	secretspg "github.com/techorionai/constellation-platform-go/secrets/postgres"
)

func ExampleMigrations() {
	entries, err := fs.ReadDir(secretspg.Migrations(), ".")
	if err != nil {
		fmt.Println(err)
		return
	}
	for _, e := range entries {
		fmt.Println(e.Name())
	}
	// Output:
	// 000001_create_platform_secrets.down.sql
	// 000001_create_platform_secrets.up.sql
}

// ExampleNewRepository has no Output comment, because it needs a PostgreSQL
// server.
func ExampleNewRepository() {
	ctx := context.Background()

	var cfg struct {
		Database clientpg.Config `envPrefix:"DATABASE__"`
		Secrets  secrets.Config  `envPrefix:"SECRETS__"`
	}
	if err := config.Load("IDENTITY_", &cfg); err != nil {
		log.Fatal(err)
	}
	db, err := clientpg.New(ctx, cfg.Database)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Run the secrets migrations with their own migrations table. The pool
	// needs MaxConns of 2 or more.
	migrator, err := pgmigrate.NewMigrator(db.Pool, secretspg.Migrations(),
		pgmigrate.MigratorOptions{MigrationsTable: secretspg.MigrationsTable})
	if err != nil {
		log.Fatal(err)
	}
	_, err = migrate.Run(ctx, pgmigrate.NewLocker(db.Pool, secretspg.MigrationsTable), migrator,
		migrate.Options{Name: secretspg.MigrationsTable})
	if err != nil {
		log.Fatal(err)
	}

	ring, err := secrets.NewKeyRing(cfg.Secrets)
	if err != nil {
		log.Fatal(err)
	}
	svc := secrets.NewService(ring, secretspg.NewRepository(db.Pool))
	if err := svc.Put(ctx, "llm/api-key", []byte("value")); err != nil {
		log.Fatal(err)
	}
}
