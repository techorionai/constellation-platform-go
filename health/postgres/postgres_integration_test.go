//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	clientpg "github.com/techorionai/constellation-platform-go/clients/postgres"
	healthpg "github.com/techorionai/constellation-platform-go/health/postgres"
	"github.com/techorionai/constellation-platform-go/resources/test/containers"
)

// I8: the container is not shared with other tests.
func TestIntegrationCheckStoppedServer(t *testing.T) {
	pg := containers.Postgres(t)
	dbURL, name := pg.NewDatabase(t)
	c, err := clientpg.New(t.Context(), clientpg.Config{URL: dbURL, Database: name, MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	checker, err := healthpg.NewChecker(t.Context(), c.Pool)
	if err != nil {
		t.Fatal(err)
	}
	defer checker.Close()
	if checker.Name() != "postgres" {
		t.Fatalf("name = %q", checker.Name())
	}
	if err := checker.Check(t.Context()); err != nil {
		t.Fatalf("first check: %v", err)
	}
	if err := pg.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := checker.Check(ctx); err == nil {
		t.Fatal("check after stop: want error")
	}
}

// I17
func TestIntegrationFullServicePool(t *testing.T) {
	pg := containers.Postgres(t)
	dbURL, name := pg.NewDatabase(t)
	c, err := clientpg.New(t.Context(), clientpg.Config{URL: dbURL, Database: name, MaxConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	held, err := c.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	checker, err := healthpg.NewChecker(t.Context(), c.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := checker.Check(t.Context()); err != nil {
		t.Fatalf("check with a full service pool: %v", err)
	}
	checker.Close()

	held.Release()
	var one int
	if err := c.Pool.QueryRow(t.Context(), "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("the service pool does not work after Close: %v", err)
	}
	// The dedicated pool has no open connection after Close. The server sees
	// only the connection of the service pool.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := c.Pool.QueryRow(t.Context(),
			"SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("connections in the database = %d, want 1", n)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
