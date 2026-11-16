package postgres_test

import (
	"context"
	"log"
	"testing/fstest"

	clientpg "github.com/techorionai/constellation-platform-go/clients/postgres"
	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/migrate"
	migratepg "github.com/techorionai/constellation-platform-go/migrate/postgres"
)

// ExampleNewMigrator shows the migrations at startup. It has no Output comment,
// because it needs a PostgreSQL server. A service embeds its files with
// //go:embed migrations/*.sql and passes fs.Sub(files, "migrations").
func ExampleNewMigrator() {
	ctx := context.Background()

	var cfg struct {
		Database clientpg.Config `envPrefix:"DATABASE__"`
	}
	if err := config.Load("IDENTITY_", &cfg); err != nil {
		log.Fatal(err)
	}
	db, err := clientpg.New(ctx, cfg.Database)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	files := fstest.MapFS{
		"000001_create_users.up.sql":   {Data: []byte("CREATE TABLE users (id uuid PRIMARY KEY);")},
		"000001_create_users.down.sql": {Data: []byte("DROP TABLE users;")},
	}
	migrator, err := migratepg.NewMigrator(db.Pool, files, migratepg.MigratorOptions{})
	if err != nil {
		log.Fatal(err)
	}
	locker := migratepg.NewLocker(db.Pool, "")
	if _, err := migrate.Run(ctx, locker, migrator, migrate.Options{}); err != nil {
		log.Fatal(err)
	}
}
