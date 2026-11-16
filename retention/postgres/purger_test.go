package postgres_test

import (
	"testing"

	retpg "github.com/techorionai/constellation-platform-go/retention/postgres"
)

// I12 needs no database, because NewTablePurger checks the names before it
// runs any SQL. The checks run before the check of the pool, so a nil pool
// shows that no SQL runs.
func TestNewTablePurgerRejectsBadNames(t *testing.T) {
	cases := map[string]retpg.TableSpec{
		"table with SQL":      {Table: "events; DROP TABLE x", TimeColumn: "created_at"},
		"upper case time col": {Table: "events", TimeColumn: "A"},
		"key with paren":      {Table: "events", TimeColumn: "created_at", KeyColumn: "id)--"},
		"schema with quote":   {Schema: `public"`, Table: "events", TimeColumn: "created_at"},
		"empty table":         {TimeColumn: "created_at"},
		"empty time column":   {Table: "events"},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := retpg.NewTablePurger(nil, spec); err == nil {
				t.Fatal("no error")
			}
		})
	}
}
