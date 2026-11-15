// Package postgres creates a PostgreSQL connection pool with tracing and pool
// metrics.
//
// Each service has its own database and its own database user. [Config] needs
// the name of the database. [New] refuses the system databases and invalid
// names, and it checks SELECT current_database() after it connects. Other
// packages that need a connection string must use [Config.ConnString].
//
// The pool uses github.com/jackc/pgx/v5/pgxpool and
// github.com/exaring/otelpgx for traces. The pool metrics use the global meter
// provider. No error contains the URL or the password.
package postgres
