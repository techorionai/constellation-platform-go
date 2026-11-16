// Package postgres is the PostgreSQL checker of the package health.
//
// [NewChecker] makes a dedicated pool with one connection. The pool uses the
// same database and user as the pool of the service, so a full service pool does
// not make the check fail, and a real PostgreSQL failure does. Each instance of
// a service uses one more connection for the checker.
package postgres
