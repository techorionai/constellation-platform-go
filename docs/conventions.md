# Conventions

These rules apply to this library and to each service that uses it.

## Package layout

`platform-go` is a library. Other repositories import its packages, so the code is not in `internal/`.

- The root package of a concern holds the port interfaces and the logic that has no infrastructure imports.
- An adapter is in a subpackage, for example `health/postgres`. An adapter imports its port package. It does not import other adapters.
- Each adapter has a compile-time interface check.
- `clients/` holds the low-level constructors for infrastructure clients.
- `resources/test/containers/` and `resources/test/mock/` hold the test helpers. They are normal packages, so that service repositories can import them.
- The `depguard` rules in `.golangci.yml` enforce these rules.

## Package documentation

Each package has documentation in two places.

1. Go documentation in the package:
   - A `doc.go` file with a package comment. The comment gives the purpose of the package, the main types and functions, and a short sample.
   - A doc comment on each exported type, function, method, and constant.
   - One or more `Example` functions in an `example_test.go` file.
2. One section for each package in [usage.md](usage.md), with these parts: purpose, import path, environment variables, use, errors, and rules and limits.

`usage.md` starts with a table of all packages.

## Environment variables

- Configuration comes from environment variables only. There are no config files.
- Each service gives one prefix, for example `IDENTITY_`. The prefix matches `^[A-Z][A-Z0-9]*_$`.
- A double underscore `__` separates the levels of a nested struct. Example: `IDENTITY_DATABASE__URL` sets `Database.URL`.
- A field with the tag `envFile:"true"` can also come from a file. The variable `<NAME>_FILE` gives the path. The loader removes one trailing line break from the file content.
- Only a field with `envFile:"true"` gets `_FILE` support. A variable such as `LOG_FILE` is a normal value.
- If `<NAME>` and `<NAME>_FILE` both have a value, the load fails.
- The OpenTelemetry settings use the standard `OTEL_*` variables without a prefix.
- The log level of the application comes from `<PREFIX>LOG_LEVEL`. The service loads it with `config.Load` and gives it to `telemetry.Options.LogLevel`. `OTEL_LOG_LEVEL` is only for the internal logger of the OpenTelemetry SDK.
- Use `config.Secret` for each secret value. Its text, JSON, and log output is `[redacted]`. Call `Reveal()` only where the code needs the value.
- An error of `config.Load` names the variable. It never contains the value.

## Data isolation

Each service has its own PostgreSQL database and its own Valkey key prefix.

- PostgreSQL: each service has one database and one database user. The user has access to its own database only. A connection string for another package comes from `postgres.Config.ConnString`, and not from `URL`.
- Valkey: each service has one key prefix and one ACL user. The ACL user has the key pattern `~<prefix>:*` and the command rule `+@all -@admin -@dangerous -function -script|flush -script|kill`.
- All keys come from `Keyspace`. A key part from outside input must not contain `:`. The caller checks it.
- A key name never contains a secret value, for example a session token. Use a hash of the secret. `SCAN` and `RANDOMKEY` return the key names of other services, also when the user has a key pattern.

See [usage.md](usage.md) for the sample SQL and the sample `ACL SETUSER` command.

## Migrations

- Each service runs its own migrations at startup with `migrate.Run`.
- Files: `migrations/NNNNNN_<slug>.up.sql` and `.down.sql`, embedded with `embed.FS`. Pass `fs.Sub(files, "migrations")` to `migrate/postgres.NewMigrator`.
- Write no secrets and no personal data in migration files, because the error of a failed migration can contain the SQL text and the database detail. `migrate.Run` writes this error to the log.
- Each migration works with the current version and the previous version of the service. Expand first, contract in a later release.
- An instance of the previous version starts when the database has a newer version. `migrate.Run` applies nothing and writes a `warn` log record. This supports rolling upgrades and rollbacks.
- golang-migrate sends each file in one call. PostgreSQL runs a call with more than one statement in one implicit transaction. A file with `CREATE INDEX CONCURRENTLY` must have only that statement.
- A stop signal during a migration lets the current migration complete. The grace period of the orchestrator must be longer than the longest single migration.
- A dirty database stops the service. The operator fixes the database and runs `migrate force <version>` by hand. See [usage.md](usage.md#migratepostgres).
- Lock key: the advisory lock key is the 64-bit FNV-1a hash of `constellation:migrate:` plus the name of the migration set. `NewLocker`, `MigratorOptions.MigrationsTable`, and `Options.Name` use the same name for one migration set. The lock and the migrations use the same database.
- The service pool has `MaxConns` of 2 or more, because `Run` uses two connections at the same time.

## Readiness and the admin port

- Each service runs an admin server (`health.Server`) on a separate port. The default is `:9090`. Do not publish the admin port at the edge. The admin server has no authentication.
- `/livez` is `200` while the process runs. `/readyz` is `503` until the service calls `State.MarkStarted()` after the migrations and the startup work. It is `503` again after `State.MarkDraining()`.
- Start the admin server before `migrate.Run`, so that the liveness probe gets `200` during a long migration. Stop it last.
- A `503` body is a problem document. It never contains the error text of a checker. The handler writes that text to the log.
- The PostgreSQL checker uses a dedicated pool of one connection for each instance. Include it in `max_connections`.
- `DrainDelay` must be longer than the probe period multiplied by the failure threshold. See [usage.md](usage.md#health).

## OpenTelemetry

Resource attributes:

| Attribute | Source |
| --------- | ------ |
| `service.name` | `Options.ServiceName`. It wins over `OTEL_SERVICE_NAME`. |
| `service.version` | `Options.ServiceVersion`. |
| `service.instance.id` | `OTEL_RESOURCE_ATTRIBUTES`, or a random UUID for each process. |

Log fields: each JSON record has `time`, `level`, `msg`, and the attributes of the call. A record in an active span also has `trace_id` and `span_id`.

A service that does not set an `OTEL_*` exporter variable makes no outbound connection.

## Metrics

PostgreSQL pool metrics. All have the attribute `db.client.connection.pool.name`.

| Metric | Meaning |
| ------ | ------- |
| `db.client.connection.count` | Connections by `db.client.connection.state` (`idle`, `used`). |
| `db.client.connection.max` | `MaxConns`. |
| `db.client.connection.idle.min` | `MinConns`. |
| `pgxpool.connection.constructing` | Connections that the pool makes now. |
| `pgxpool.acquires` | Acquires by `pgxpool.acquire.result`. |
| `pgxpool.acquire.time` | Total time of the acquires, in seconds. |
| `pgxpool.connections.created` | Connections that the pool made. |
| `pgxpool.connections.closed` | Connections that the pool closed, by `pgxpool.close.reason` (`max_lifetime`, `max_idle`). |

Values of `pgxpool.acquire.result`:

- `idle`: the acquire got an idle connection immediately.
- `empty`: the acquire waited, because the pool had no idle connection. With `MinConns` 0, `empty` also goes up at normal load.
- `canceled`: the context of the acquire ended before it got a connection.

A high rate of `empty` while `used` is equal to `max`, or a high rate of `canceled`, shows that `MaxConns` is too low. The mean wait for a connection is the rate of `pgxpool.acquire.time` divided by the rate of `pgxpool.acquires`.

Migration metric: `migrate.duration` (histogram, unit `s`) with the attributes `migrate.name` and `outcome`. See [usage.md](usage.md#migrate).

Valkey connection metrics come from `valkeyotel`: `valkey_dial_attempt`, `valkey_dial_success`, `valkey_dial_conns`, and `valkey_dial_latency`. See [usage.md](usage.md).

## Tests

- Integration tests have the build tag `integration`: `//go:build integration`. Run them with `go test -race -tags=integration ./...`.
- Server-side integration tests use the test containers of `resources/test/containers` and live dependencies. They do not use mocks or stubs.
- A mock goes in `resources/test/mock` and replaces a port only in a unit test.
- A unit test that imports `resources/test/mock` is in the external test package (`<package>_test`), because the mocks import the package under test.
- Put the test ID of the plan in the test name or in a comment above the test.
