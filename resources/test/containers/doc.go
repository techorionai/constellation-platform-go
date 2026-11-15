// Package containers starts PostgreSQL and Valkey containers for tests.
//
// [Postgres] and [Valkey] start a container, wait for it, and stop it at the
// end of the test with t.Cleanup. Tests in one package can share one container.
// [PostgresInstance.NewDatabase] gives each test its own database.
// [ValkeyInstance.NewACLUser] makes an ACL user with a key prefix rule.
//
// The package has no build tag, so that service repositories can import it. It
// imports the testing package. Only test code must import it. Docker must be
// available.
package containers
