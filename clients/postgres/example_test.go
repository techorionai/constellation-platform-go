package postgres_test

import (
	"context"
	"fmt"
	"log"

	"github.com/techorionai/constellation-platform-go/clients/postgres"
	"github.com/techorionai/constellation-platform-go/config"
)

func ExampleConfig_ConnString() {
	cfg := postgres.Config{ //nolint:gosec // sample data
		URL:      "postgres://identity:password@db.internal:5432",
		Database: "identity",
	}
	connString, err := cfg.ConnString()
	if err != nil {
		fmt.Println(err)
		return
	}
	// The result is a config.Secret, so the output is redacted.
	fmt.Println(connString)
	// Output: [redacted]
}

// ExampleNew shows the startup of a service. It has no Output comment, because
// it needs a PostgreSQL server.
func ExampleNew() {
	ctx := context.Background()

	var cfg struct {
		Database postgres.Config `envPrefix:"DATABASE__"`
	}
	if err := config.Load("IDENTITY_", &cfg); err != nil {
		log.Fatal(err)
	}
	client, err := postgres.New(ctx, cfg.Database)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	var name string
	if err := client.Pool.QueryRow(ctx, "SELECT current_database()").Scan(&name); err != nil {
		log.Fatal(err)
	}
	fmt.Println(name)
}
