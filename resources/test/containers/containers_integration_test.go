//go:build integration

package containers_test

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/techorionai/constellation-platform-go/resources/test/containers"
)

// I5
func TestIntegrationParallelDatabases(t *testing.T) {
	pg := containers.Postgres(t)
	var names [2]string
	t.Run("group", func(t *testing.T) {
		for i := range names {
			t.Run("db", func(t *testing.T) {
				t.Parallel()
				dbURL, name := pg.NewDatabase(t)
				names[i] = name
				conn, err := pgx.Connect(t.Context(), dbURL.Reveal())
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close(t.Context()) //nolint:errcheck
				if _, err := conn.Exec(t.Context(), "CREATE TABLE t1 (id int)"); err != nil {
					t.Fatal(err)
				}
				var n int
				if err := conn.QueryRow(t.Context(),
					"SELECT count(*) FROM information_schema.tables WHERE table_name = 't1'").Scan(&n); err != nil || n != 1 {
					t.Fatalf("t1 count = %d, err = %v", n, err)
				}
			})
		}
	})
	conn, err := pgx.Connect(t.Context(), pg.AdminURL().Reveal())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context()) //nolint:errcheck
	for _, name := range names {
		var exists bool
		if err := conn.QueryRow(t.Context(),
			"SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Errorf("database %s still exists", name)
		}
	}
}
