package postgres

import "testing"

// U23
func TestLockKey(t *testing.T) {
	name := "schema_migrations"
	if lockKey(name) != lockKey(name+"") {
		t.Fatal("the same name gives different keys")
	}
	if lockKey("schema_migrations") == lockKey("platform_secrets_schema_migrations") {
		t.Fatal("different names give the same key")
	}
	if lockKey("") != lockKey("schema_migrations") {
		t.Fatal("an empty name does not give the key of schema_migrations")
	}
}
