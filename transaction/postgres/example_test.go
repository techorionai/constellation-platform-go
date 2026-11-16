package postgres_test

import (
	"context"
	"log"

	clientpg "github.com/techorionai/constellation-platform-go/clients/postgres"
	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/transaction"
	txpg "github.com/techorionai/constellation-platform-go/transaction/postgres"
)

// ExampleNewTransactor has no Output comment, because it needs a PostgreSQL
// server.
func ExampleNewTransactor() {
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

	tx := txpg.NewTransactor(db.Pool)
	err = tx.WithinTx(ctx, func(ctx context.Context) error {
		// A repository gets its connection with Conn for each call. It joins
		// this transaction.
		_, err := clientpg.Conn(ctx, db.Pool).Exec(ctx, "INSERT INTO items (id) VALUES ($1)", "i1")
		if err != nil {
			return err
		}
		tx.AfterCommit(ctx, func(context.Context) { log.Print("committed") })
		return nil
	}, transaction.Name("create_item"))
	if err != nil {
		log.Fatal(err)
	}
}
