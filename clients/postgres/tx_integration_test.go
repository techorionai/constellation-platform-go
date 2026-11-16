//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/techorionai/constellation-platform-go/clients/postgres"
	"github.com/techorionai/constellation-platform-go/resources/test/containers"
	txpg "github.com/techorionai/constellation-platform-go/transaction/postgres"
)

// I12
func TestIntegrationConnJoinsTransaction(t *testing.T) {
	pg := containers.Postgres(t)
	dbURL, name := pg.NewDatabase(t)
	c, err := postgres.New(t.Context(), postgres.Config{URL: dbURL, Database: name, MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Pool.Exec(t.Context(), `CREATE TABLE items (id text PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}

	// A repository adapter gets its connection with Conn for each call.
	save := func(ctx context.Context, id string) error {
		_, err := postgres.Conn(ctx, c.Pool).Exec(ctx, `INSERT INTO items (id) VALUES ($1)`, id)
		return err
	}
	exists := func(id string) bool {
		var n int
		if err := c.Pool.QueryRow(t.Context(), `SELECT count(*) FROM items WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}

	tx := txpg.NewTransactor(c.Pool)
	errX := errors.New("x")
	err = tx.WithinTx(t.Context(), func(ctx context.Context) error {
		if _, ok := postgres.TxFromContext(ctx); !ok {
			t.Error("the context has no transaction")
		}
		if err := save(ctx, "in-tx"); err != nil {
			return err
		}
		return errX
	})
	if !errors.Is(err, errX) {
		t.Fatalf("err = %v", err)
	}
	if err := save(t.Context(), "no-tx"); err != nil {
		t.Fatal(err)
	}
	if exists("in-tx") {
		t.Fatal("the row of the rolled back transaction exists")
	}
	if !exists("no-tx") {
		t.Fatal("the row without a transaction does not exist")
	}
}
