package postgres_test

import (
	"context"
	"log"
	"time"

	clientpg "github.com/techorionai/constellation-platform-go/clients/postgres"
	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/retention"
	retpg "github.com/techorionai/constellation-platform-go/retention/postgres"
)

// ExampleNewTablePurger has no Output comment, because it needs a PostgreSQL
// server.
func ExampleNewTablePurger() {
	ctx := context.Background()

	var cfg struct {
		Database       clientpg.Config  `envPrefix:"DATABASE__"`
		AuditRetention retention.Config `envPrefix:"RETENTION__AUDIT__"`
	}
	if err := config.Load("IDENTITY_", &cfg); err != nil {
		log.Fatal(err)
	}
	// The pool needs MaxConns of 2 or more. Each tick of a job uses 2
	// connections, so leave spare connections for the jobs.
	db, err := clientpg.New(ctx, cfg.Database)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	purger, err := retpg.NewTablePurger(db.Pool, retpg.TableSpec{Table: "audit_entries", TimeColumn: "created_at"})
	if err != nil {
		log.Fatal(err)
	}
	locker, err := retpg.NewLocker(db.Pool)
	if err != nil {
		log.Fatal(err)
	}
	runner, err := retention.NewRunner(locker, []retention.Job{{
		Name:          "audit",
		Config:        cfg.AuditRetention,
		DefaultPeriod: 60 * 24 * time.Hour,
		Purger:        purger,
	}}, retention.RunnerOptions{})
	if err != nil {
		log.Fatal(err)
	}
	// Run stops and returns when ctx ends.
	go func() { _ = runner.Run(ctx) }()
}
