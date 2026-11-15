package containers_test

import (
	"testing"

	"github.com/techorionai/constellation-platform-go/resources/test/containers"
)

// ExamplePostgres shows the use in a test. It has no Output comment, because it
// needs Docker. In a real test, t is the argument of the test function.
func ExamplePostgres() {
	var t testing.TB

	pg := containers.Postgres(t)
	dbURL, database := pg.NewDatabase(t) // a new database for this test
	_, _ = dbURL, database
}

// ExampleValkey shows the use in a test. It has no Output comment, because it
// needs Docker. In a real test, t is the argument of the test function.
func ExampleValkey() {
	var t testing.TB

	v := containers.Valkey(t)
	user, password := v.NewACLUser(t, "identity") // an ACL user with the key prefix rule
	_, _, _ = v.Address(), user, password
}
