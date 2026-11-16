package postgres_test

import (
	"context"
	"log"

	clientpg "github.com/techorionai/constellation-platform-go/clients/postgres"
	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/health"
	healthpg "github.com/techorionai/constellation-platform-go/health/postgres"
)

// ExampleNewChecker has no Output comment, because it needs a PostgreSQL
// server.
func ExampleNewChecker() {
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

	check, err := healthpg.NewChecker(ctx, db.Pool)
	if err != nil {
		log.Fatal(err)
	}
	defer check.Close()

	state := health.NewState(check)
	state.MarkStarted()
}
