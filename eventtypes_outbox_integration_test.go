//go:build integration

package platform_test

import (
	"context"
	"errors"
	"testing"
	"testing/fstest"

	clientpg "github.com/techorionai/constellation-platform-go/clients/postgres"
	"github.com/techorionai/constellation-platform-go/eventtypes"
	"github.com/techorionai/constellation-platform-go/migrate"
	migratepg "github.com/techorionai/constellation-platform-go/migrate/postgres"
	"github.com/techorionai/constellation-platform-go/outbox"
	outboxpg "github.com/techorionai/constellation-platform-go/outbox/postgres"
	"github.com/techorionai/constellation-platform-go/resources/test/containers"
	txpg "github.com/techorionai/constellation-platform-go/transaction/postgres"
)

// Issue 22 I1: the outbox writer gets its app and its types from a registration
// file. A registered type commits. A type that is not registered gives
// ErrUnknownEventType and the business change rolls back.
func TestI1OutboxWriterFromRegistration(t *testing.T) {
	pg := containers.Postgres(t)
	dbURL, name := pg.NewDatabase(t)
	c, err := clientpg.New(t.Context(), clientpg.Config{URL: dbURL, Database: name, MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	migrator, err := migratepg.NewMigrator(c.Pool, outboxpg.Migrations(),
		migratepg.MigratorOptions{MigrationsTable: outboxpg.MigrationsTable})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Run(t.Context(), migratepg.NewLocker(c.Pool, outboxpg.MigrationsTable), migrator,
		migrate.Options{Name: outboxpg.MigrationsTable}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Pool.Exec(t.Context(), "CREATE TABLE business (id int PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}

	reg, err := eventtypes.Load(fstest.MapFS{"event-types.json": {Data: []byte(
		`{"source":"/helpdesk","app":"helpdesk","eventTypes":[{"type":"helpdesk.ticket.created","description":"Created.","dataSchema":{"type":"object"}}]}`)}},
		"event-types.json")
	if err != nil {
		t.Fatal(err)
	}
	writer, err := outboxpg.NewWriter(c.Pool, outbox.Config{App: reg.App, Types: reg.Types()})
	if err != nil {
		t.Fatal(err)
	}
	tx := txpg.NewTransactor(c.Pool)
	write := func(id int, typ string) error {
		return tx.WithinTx(t.Context(), func(ctx context.Context) error {
			q := clientpg.Conn(ctx, c.Pool)
			if _, err := q.Exec(ctx, "INSERT INTO business (id) VALUES ($1)", id); err != nil {
				return err
			}
			return writer.Write(ctx, outbox.Event{Type: typ, Data: map[string]any{}, Actor: outbox.ServiceActor("svc")})
		})
	}
	count := func(table string) int {
		var n int
		if err := c.Pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if err := write(1, "helpdesk.ticket.created"); err != nil {
		t.Fatal(err)
	}
	if err := write(2, "helpdesk.ticket.deleted"); !errors.Is(err, outbox.ErrUnknownEventType) {
		t.Fatalf("err = %v, want ErrUnknownEventType", err)
	}
	if n := count("business"); n != 1 {
		t.Errorf("business rows = %d, want 1", n)
	}
	if n := count("platform_outbox"); n != 1 {
		t.Errorf("outbox rows = %d, want 1", n)
	}
}
