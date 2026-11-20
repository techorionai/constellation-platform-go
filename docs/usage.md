# Usage

Install a release:

```sh
go get github.com/techorionai/constellation-platform-go@vX.Y.Z
```

## Packages

| Package | Purpose |
| ------- | ------- |
| [`config`](#config) | Load a config struct from environment variables. |
| [`telemetry`](#telemetry) | Set up OpenTelemetry traces, metrics, and logs. |
| [`clients/postgres`](#clientspostgres) | Create a PostgreSQL pool. |
| [`clients/valkey`](#clientsvalkey) | Create a Valkey client and make keys. |
| [`migrate`](#migrate) | Run the migrations at startup with a lock. |
| [`migrate/postgres`](#migratepostgres) | PostgreSQL advisory lock and golang-migrate adapters. |
| [`health`](#health) | Liveness and readiness endpoints on the admin port. |
| [`health/postgres`](#healthpostgres) | PostgreSQL checker for readiness. |
| [`health/valkey`](#healthvalkey) | Valkey checker for readiness. |
| [`executor`](#executor) | Generic executor interfaces for commands and queries. |
| [`decorators`](#decorators) | Tracing and logging decorators for executors. |
| [`cache`](#cache) | Cache port and typed cache with `GetOrLoad`. |
| [`cache/valkey`](#cachevalkey) | Valkey adapter of the cache port. |
| [`transaction`](#transaction) | Transaction port with `AfterCommit` hooks. |
| [`transaction/postgres`](#transactionpostgres) | PostgreSQL adapter of the transaction port. |
| [`secrets`](#secrets) | Envelope encryption and storage of secrets, with master key rotation. |
| [`secrets/postgres`](#secretspostgres) | PostgreSQL adapter and migrations of the secrets store. |
| [`retention`](#retention) | Background jobs that delete expired data. |
| [`retention/postgres`](#retentionpostgres) | PostgreSQL table purger and advisory lock for retention jobs. |
| [`requestid`](#requestid) | Request ID middleware. |
| [`auth`](#auth) | Principal, token verifier port, scope checks, and HTTP middleware. |
| [`auth/jwt`](#authjwt) | JWKS token verifier for RFC 9068 access tokens. |
| [`resources/test/containers`](#resourcestestcontainers) | Start PostgreSQL and Valkey containers in tests. |
| [`resources/test/issuer`](#resourcestestissuer) | Test token issuer with a JWKS endpoint. |
| [`resources/test/mock`](#resourcestestmock) | Mocks of ports. |

Each sample in this file is a copy of an `Example` function of the package. The `example_test.go` files compile these samples.

## Startup sample

```go
ctx := context.Background()

var cfg struct {
	LogLevel slog.Level      `env:"LOG_LEVEL" envDefault:"INFO"`
	Database postgres.Config `envPrefix:"DATABASE__"`
	Valkey   valkey.Config   `envPrefix:"VALKEY__"`
}
if err := config.Load("IDENTITY_", &cfg); err != nil {
	log.Fatal(err)
}
tel, err := telemetry.Setup(ctx, telemetry.Options{
	ServiceName: "identity", ServiceVersion: "1.0.0", LogLevel: cfg.LogLevel,
})
if err != nil {
	log.Fatal(err)
}
defer tel.Shutdown(ctx)

db, err := postgres.New(ctx, cfg.Database)
if err != nil {
	log.Fatal(err)
}
defer db.Close()

kv, err := valkey.New(ctx, cfg.Valkey)
if err != nil {
	log.Fatal(err)
}
defer kv.Close()
```

## Startup order

A service with migrations and the admin server follows this order. A failed connection in step 3 writes an `error` log record and stops the service with a non-zero exit code. The orchestrator starts it again. `clients/postgres` and `clients/valkey` have a connect timeout, so this step does not wait for a long time. The admin server already runs during `migrate.Run`, so the liveness probe gets `200` during a long migration.

```go
ctx := context.Background()

var cfg struct {
	LogLevel slog.Level      `env:"LOG_LEVEL" envDefault:"INFO"`
	Database postgres.Config `envPrefix:"DATABASE__"`
	Valkey   valkey.Config   `envPrefix:"VALKEY__"`
	Admin    health.Config   `envPrefix:"ADMIN__"`
	Auth     authjwt.Config  `envPrefix:"AUTH__"`
}
// 1. Load the config.
if err := config.Load("IDENTITY_", &cfg); err != nil {
	log.Fatal(err)
}
// 2. Set up OpenTelemetry.
tel, err := telemetry.Setup(ctx, telemetry.Options{
	ServiceName: "identity", ServiceVersion: "1.0.0", LogLevel: cfg.LogLevel,
})
if err != nil {
	log.Fatal(err)
}
// 3. Connect to PostgreSQL and Valkey, and make the checkers. A failed
// connection stops the service with a non-zero exit code.
db, err := postgres.New(ctx, cfg.Database)
if err != nil {
	log.Fatal(err)
}
kv, err := valkey.New(ctx, cfg.Valkey)
if err != nil {
	log.Fatal(err)
}
pgCheck, err := healthpg.NewChecker(ctx, db.Pool)
if err != nil {
	log.Fatal(err)
}

// 4. Start the admin server. /livez is 200. /readyz is 503.
state := health.NewState(pgCheck, healthvk.NewChecker(kv.Raw))
admin := health.NewServer(cfg.Admin, state)
go func() {
	if err := admin.ListenAndServe(); err != nil {
		log.Print(err)
	}
}()

// 5. Run the migrations. A service embeds its files with
// //go:embed migrations/*.sql and passes fs.Sub(files, "migrations").
files := fstest.MapFS{
	"000001_init.up.sql": {Data: []byte("CREATE TABLE users (id uuid PRIMARY KEY);")},
}
migrator, err := migratepg.NewMigrator(db.Pool, files, migratepg.MigratorOptions{})
if err != nil {
	log.Fatal(err)
}
if _, err := migrate.Run(ctx, migratepg.NewLocker(db.Pool, ""), migrator, migrate.Options{}); err != nil {
	log.Fatal(err)
}

// 6. Start the background jobs and the API server. The verifier fetches the
// JWKS on the first request, so the service starts when identity is down.
verifier, err := authjwt.New(cfg.Auth)
if err != nil {
	log.Fatal(err)
}
verifier.Start(ctx)
mux := http.NewServeMux()
// Protect each route: auth.RequireScopes("demo:ticket:read")(handler)
api := &http.Server{
	Addr:              ":8080",
	Handler:           requestid.Middleware()(auth.Middleware(verifier)(mux)),
	ReadHeaderTimeout: 5 * time.Second,
}
go func() {
	if err := api.ListenAndServe(); err != nil {
		log.Print(err)
	}
}()
// 7. Tell the state that the service is ready.
state.MarkStarted()

// 8. On a stop signal, do these steps in this order:
_ = admin.Drain(ctx)  // the load balancer sees 503 during DrainDelay
_ = api.Shutdown(ctx) // stop the API server
verifier.Close()      // stop the JWKS refresh and the other jobs
pgCheck.Close()
kv.Close()
db.Close()
_ = tel.Shutdown(ctx)
_ = admin.Shutdown(ctx) // the admin server stops last
```

## config

Purpose: load a config struct from environment variables. The package also gives `Secret`, a string type that does not show its value.

Import path: `github.com/techorionai/constellation-platform-go/config`

Environment variables: None. The struct of the caller gives the variables.

Use:

```go
_ = os.Setenv("IDENTITY_NAME", "identity")
_ = os.Setenv("IDENTITY_DATABASE__URL", "postgres://u:p@localhost/db")

type Database struct {
	URL config.Secret `env:"URL,required" envFile:"true"`
}
var cfg struct {
	Name     string   `env:"NAME,required"`
	Database Database `envPrefix:"DATABASE__"`
}
if err := config.Load("IDENTITY_", &cfg); err != nil {
	fmt.Println(err)
	return
}
fmt.Println(cfg.Name, cfg.Database.URL)
// Output: identity [redacted]
```

Errors:

- `ErrInvalidPrefix`: the prefix does not match `^[A-Z][A-Z0-9]*_$`.
- Other errors name the variable and never contain the value. `Load` collects all errors into one error.

Rules and limits:

- Use the tags of `github.com/caarlos0/env/v11`: `env`, `envDefault`, and `envPrefix`. Do not use the `file` tag option.
- Only a field with `envFile:"true"` reads `<NAME>_FILE`.
- If `<NAME>` and `<NAME>_FILE` both have a value, `Load` fails.
- The config structs of the services stay in each service.

## telemetry

Purpose: set up OpenTelemetry traces, metrics, and logs, and the default `slog` logger.

Import path: `github.com/techorionai/constellation-platform-go/telemetry`

Environment variables: the standard `OTEL_*` variables. There is no prefix.

| Name | Type | Default | Use |
| ---- | ---- | ------- | --- |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | URL | None | Turns on the OTLP exporters for all signals. Optional. |
| `OTEL_EXPORTER_OTLP_<SIGNAL>_ENDPOINT` | URL | None | Turns on the OTLP exporter for one signal. `<SIGNAL>` is `TRACES`, `METRICS`, or `LOGS`. Optional. |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | string | `http/protobuf` | Optional. |
| `OTEL_TRACES_EXPORTER`, `OTEL_METRICS_EXPORTER`, `OTEL_LOGS_EXPORTER` | string | None | Select an exporter, for example `console` or `none`. Optional. |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | string | `parentbased_always_on` | Optional. |
| `OTEL_RESOURCE_ATTRIBUTES` | string | None | Extra resource attributes. Optional. |
| `OTEL_SERVICE_NAME` | string | None | `Options.ServiceName` wins over it. Optional. |

Use:

```go
ctx := context.Background()
p, err := telemetry.Setup(ctx, telemetry.Options{
	ServiceName:    "identity",
	ServiceVersion: "1.0.0",
	LogLevel:       slog.LevelInfo,
})
if err != nil {
	fmt.Println(err)
	return
}
defer func() { _ = p.Shutdown(ctx) }()
fmt.Println("ready")
// Output: ready
```

Errors:

- `ErrMissingServiceName`: `Options.ServiceName` is empty.

Rules and limits:

- A signal gets an exporter only if `OTEL_<SIGNAL>_EXPORTER`, `OTEL_EXPORTER_OTLP_ENDPOINT`, or `OTEL_EXPORTER_OTLP_<SIGNAL>_ENDPOINT` has a value. Without these variables, the service makes no outbound connection.
- Call `Shutdown` before the process stops. A second call returns nil.
- `Shutdown` also resets the global state that `Setup` set, if this `Provider` is the one of the last `Setup` call. The global tracer, meter, and logger providers become no-op providers, the global propagator becomes empty, and the default `slog` logger returns to the logger from before `Setup`.
- `Setup` sets global state. Call it one time for each process.
- The log level comes from `Options.LogLevel`. `OTEL_LOG_LEVEL` does not change it.

## clients/postgres

Purpose: create a `pgxpool` connection pool with tracing, pool metrics, and a required database name.

Import path: `github.com/techorionai/constellation-platform-go/clients/postgres`

Environment variables: nest `postgres.Config` with `envPrefix:"DATABASE__"`.

| Name | Type | Default | Use |
| ---- | ---- | ------- | --- |
| `<PREFIX>DATABASE__URL` | secret | None | Required. Also `<PREFIX>DATABASE__URL_FILE`. |
| `<PREFIX>DATABASE__NAME` | string | None | Required. Matches `^[a-z_][a-z0-9_]{0,62}$`. |
| `<PREFIX>DATABASE__MAX_CONNS` | int | `10` | Optional. |
| `<PREFIX>DATABASE__MIN_CONNS` | int | `0` | Optional. |
| `<PREFIX>DATABASE__CONNECT_TIMEOUT` | duration | `5s` | Optional. |

Use:

```go
ctx := context.Background()

var cfg struct {
	Database postgres.Config `envPrefix:"DATABASE__"`
}
if err := config.Load("IDENTITY_", &cfg); err != nil {
	log.Fatal(err)
}
client, err := postgres.New(ctx, cfg.Database)
if err != nil {
	log.Fatal(err)
}
defer client.Close()

var name string
if err := client.Pool.QueryRow(ctx, "SELECT current_database()").Scan(&name); err != nil {
	log.Fatal(err)
}
fmt.Println(name)
```

Errors:

- `ErrInvalidURL`: the URL does not start with `postgres://` or `postgresql://`, or the URL has a query key that changes the target of the connection (see the rules below).
- `ErrInvalidDatabaseName`: the name is empty or does not match the pattern.
- `ErrSystemDatabase`: the name is `postgres`, `template0`, or `template1`.
- `ErrDatabaseMismatch`: the database in the URL, or `current_database()`, is not `Database`.

No error contains the URL or the password.

Rules and limits:

- Accepted URL forms: `postgres://` and `postgresql://` URLs only. A keyword/value string, for example `host=a dbname=b`, is not accepted.
- The URL must not have the query keys `dbname`, `database`, `host`, `hostaddr`, `port`, `service`, or `servicefile` (the check ignores letter case). `pgx` lets these keys override the parts of the URL, so `ConnString` and `New` return `ErrInvalidURL` for them. The error names the key and never contains the value.
- Production TLS rule: use `sslmode=verify-full` in the URL. `New` writes a `warn` log record if the URL has no `sslmode`, or has `disable`, `allow`, or `prefer`. With these values, an attacker on the network can force plain text and read the password. The package does not require TLS, because the URL sets the TLS mode.
- Each service has its own database user, and the user has access to its own database only. Sample SQL:

  ```sql
  CREATE ROLE identity LOGIN PASSWORD '<password>';
  CREATE DATABASE identity OWNER identity;
  REVOKE CONNECT ON DATABASE identity FROM PUBLIC;
  GRANT CONNECT ON DATABASE identity TO identity;
  ```

- Do not set `PG*` variables (for example `PGHOST` or `PGDATABASE`) for a service. `pgxpool` uses them as defaults for the parts that the URL does not give. `PGDATABASE` has no effect on the database, but other variables can fill other parts of the connection.
- Another package that needs a connection string uses `Config.ConnString()`. It does not use `URL` directly.
- `Conn(ctx, pool)` returns the transaction in `ctx`, or the pool if `ctx` has no transaction. Both have the type `Querier` (`Exec`, `Query`, `QueryRow`, and `SendBatch`). Each repository adapter gets its connection with `postgres.Conn(ctx, r.pool)` for each call, so that it joins the transaction of the caller. A repository that uses the pool directly does not join the transaction.
- `ContextWithTx` and `TxFromContext` carry the transaction in the context. Only the transaction adapter ([`transaction/postgres`](#transactionpostgres)) calls `ContextWithTx`.
- `Client.Pool` is the `*pgxpool.Pool`. `Client.Close()` stops the pool metrics, then closes the pool.
- Metrics: see the table in [conventions.md](conventions.md#metrics). The metrics use the global meter provider.

## clients/valkey

Purpose: create a `valkey-go` client with tracing, connection metrics, and a required key prefix. The `Keyspace` type makes the keys.

Import path: `github.com/techorionai/constellation-platform-go/clients/valkey`

Environment variables: nest `valkey.Config` with `envPrefix:"VALKEY__"`.

| Name | Type | Default | Use |
| ---- | ---- | ------- | --- |
| `<PREFIX>VALKEY__ADDRESS` | string | None | Required. |
| `<PREFIX>VALKEY__USERNAME` | string | Empty | Optional. An empty value means the `default` user. `New` writes a `warn` log record. |
| `<PREFIX>VALKEY__PASSWORD` | secret | Empty | Optional. Also `<PREFIX>VALKEY__PASSWORD_FILE`. |
| `<PREFIX>VALKEY__TLS` | bool | `false` | Optional. |
| `<PREFIX>VALKEY__DIAL_TIMEOUT` | duration | `5s` | Optional. |
| `<PREFIX>VALKEY__KEY_PREFIX` | string | None | Required. Matches `^[a-z][a-z0-9_]{0,31}$`. |

Use:

```go
ctx := context.Background()

var cfg struct {
	Valkey valkey.Config `envPrefix:"VALKEY__"`
}
if err := config.Load("IDENTITY_", &cfg); err != nil {
	log.Fatal(err)
}
client, err := valkey.New(ctx, cfg.Valkey)
if err != nil {
	log.Fatal(err)
}
defer client.Close()

// Make each key with the Keyspace, so that it has the key prefix.
key := client.Keys.Key("session", "42")
cmd := client.Raw.B().Set().Key(key).Value("1").Build()
if err := client.Raw.Do(ctx, cmd).Error(); err != nil {
	log.Fatal(err)
}
```

Keyspace sample:

```go
ks, err := valkey.NewKeyspace("identity")
if err != nil {
	fmt.Println(err)
	return
}
fmt.Println(ks.Key("session", "42"))
fmt.Println(ks.Pattern("session"))
// Output:
// identity:session:42
// identity:session:*
```

Errors:

- `ErrInvalidKeyPrefix`: the key prefix does not match the pattern.
- `New` returns an error that wraps the error of the context, if the context is canceled. A failed `New` leaves no open connection.

Rules and limits:

- Each service needs its own ACL user. Sample command:

  ```txt
  ACL SETUSER identity on >password ~identity:* +@all -@admin -@dangerous -function -script|flush -script|kill
  ```

  - `-@dangerous` refuses `FLUSHALL`, `FLUSHDB`, `KEYS`, and other commands of that category.
  - `-function`, `-script|flush`, and `-script|kill` refuse the commands that change or read the function libraries and the script cache. These are global for the server, and a key pattern does not apply to them.
  - `EVAL`, `EVALSHA`, and `FCALL` stay allowed, because Valkey checks their key arguments against the key pattern.
- Production TLS rule: set `<PREFIX>VALKEY__TLS` to `true` when the client has a password. `New` writes a `warn` log record if there is a password and `TLS` is `false`, because the server then reads the password in plain text. The package does not require TLS.
- The client turns off the client-side cache of `valkey-go` (`DisableCache: true`). The cache is on by default and uses server-assisted invalidation with `CLIENT TRACKING`. This package does not use client-side caching.
- If the server does not allow custom ACL users (some managed services), use one Valkey instance for each service.
- The operator loads Valkey functions with an admin user. A service does not load its own functions.
- `valkey-go` commands are typed builders, so the package cannot add the prefix. Make each key with `Client.Keys`. The ACL rule makes Valkey refuse a key without the prefix.
- A key name never contains a secret value. Test I15 shows that `SCAN 0 MATCH *` with an ACL user that has the key pattern `~identity:*` returns the key names of other services (the test result is `true`). `RANDOMKEY` can also return them. Use a hash of the secret in the key name.
- A key part from outside input must not contain `:`. `Key("a:b")` is equal to `Key("a", "b")`. The caller checks it. `Key` panics for an empty part.
- `Pattern` escapes the glob characters `*`, `?`, `[`, `]`, and `\` in each part.
- Connection metrics come from `valkeyotel` and use the global meter provider:

  | Metric | Type | Meaning |
  | ------ | ---- | ------- |
  | `valkey_dial_attempt` | Counter | Dial attempts. |
  | `valkey_dial_success` | Counter | Successful dials. |
  | `valkey_dial_conns` | UpDownCounter | Open connections. |
  | `valkey_dial_latency` | Histogram, `s` | Dial latency. |
  | `valkey_command_duration_seconds` | Histogram, `s` | Command duration. |
  | `valkey_command_errors` | Counter | Command errors. |

  `valkey-go` does not use a classic pool, so "used" and "idle" counts do not apply. `valkeyotel` also makes a span for each command.

## migrate

Purpose: run the database migrations of a service at startup. Only one instance runs the migrations. The other instances wait. The package is a port with the function `Run`. The adapters are in [`migrate/postgres`](#migratepostgres).

Import path: `github.com/techorionai/constellation-platform-go/migrate`

Environment variables: None.

Use:

```go
res, err := migrate.Run(context.Background(), sampleLocker{}, sampleMigrator{}, migrate.Options{})
if err != nil {
	fmt.Println(err)
	return
}
fmt.Println(res.Applied, res.ToVersion)
// Output: 2 3
```

`sampleLocker` and `sampleMigrator` do no database work. A service uses the adapters of `migrate/postgres`.

Errors:

- `ErrLockTimeout`: `Run` did not get the lock in `Options.LockTimeout`.
- `ErrDirty`: the database has a failed migration.
- `ErrInvalidOptions`: `Options.Name` does not match `^[a-z_][a-z0-9_]{0,62}$`, or `Options.LockTimeout` is negative. `Run` does not call `Lock`.
- If the context ends, the error wraps the error of the context.

Rules and limits:

- `Run` takes the lock, calls `Up`, and releases the lock. The release also happens when `Up` fails or when the context ends.
- `Options.LockTimeout` is the maximum wait for the lock. `0` gives 10 minutes. A waiting instance can wait longer than 15 seconds, which is the golang-migrate default.
- `Options.Name` names the migration set. An empty name gives `schema_migrations`. Use the same name for the locker, for `MigratorOptions.MigrationsTable`, and for `Options.Name`.
- The package must not import a database driver or `net/http`. The `depguard` rules enforce it.
- `Run` writes log records, makes the span `migrate.run`, and records the metric `migrate.duration` (histogram, unit `s`). All three have the attribute `migrate.name`. The metric also has the attribute `outcome`:

  | `outcome` | When |
  | --------- | ---- |
  | `ok` | `Up` returned no error and `Ahead` is false. |
  | `ahead` | `Up` returned no error and `Ahead` is true. |
  | `lock_timeout` | `Run` returned `ErrLockTimeout`. |
  | `dirty` | `Run` returned `ErrDirty`. |
  | `canceled` | The error wraps `context.Canceled` or `context.DeadlineExceeded`. |
  | `error` | All other errors. |

- On an error, the span has the status `Error` with the outcome as description, and the attribute `error.type` with the Go type of the innermost error. The span has no error event and no error text.
- The error log record of `Run` holds the full error text. The error of a failed migration can contain SQL text and database detail, for example the values of a row in a unique violation. Do not write secrets or personal data in migration files. Give read access to the logs only to people who can read the database.
- If `Result.Ahead` is true, the database version is higher than the newest version in the source. `Run` writes a `warn` log record with `from_version` and `to_version`, which are the database version. This is normal during a rolling upgrade or a rollback.

## migrate/postgres

Purpose: the PostgreSQL adapters of `migrate`. `Locker` is a session advisory lock. `Migrator` runs golang-migrate on one pool connection.

Import path: `github.com/techorionai/constellation-platform-go/migrate/postgres`

Environment variables: None.

Use:

```go
ctx := context.Background()

var cfg struct {
	Database clientpg.Config `envPrefix:"DATABASE__"`
}
if err := config.Load("IDENTITY_", &cfg); err != nil {
	log.Fatal(err)
}
db, err := clientpg.New(ctx, cfg.Database)
if err != nil {
	log.Fatal(err)
}
defer db.Close()

files := fstest.MapFS{
	"000001_create_users.up.sql":   {Data: []byte("CREATE TABLE users (id uuid PRIMARY KEY);")},
	"000001_create_users.down.sql": {Data: []byte("DROP TABLE users;")},
}
migrator, err := migratepg.NewMigrator(db.Pool, files, migratepg.MigratorOptions{})
if err != nil {
	log.Fatal(err)
}
locker := migratepg.NewLocker(db.Pool, "")
if _, err := migrate.Run(ctx, locker, migrator, migrate.Options{}); err != nil {
	log.Fatal(err)
}
```

Errors:

- `ErrPoolTooSmall`: the pool has `MaxConns` below 2.
- `NewMigrator` also returns an error if the source has no migration file, or if `MigrationsTable` does not match `^[a-z_][a-z0-9_]{0,62}$`.
- `Up` returns an error that wraps `migrate.ErrDirty` if the database is dirty. No error contains a connection string or a password, because the migrator uses the pool and gets no URL. An error of a failed migration can contain the SQL text and the database detail, for example row values. `Run` writes this text to the log. See [migrate](#migrate).

Rules and limits:

- The lock key is `int64` of the 64-bit FNV-1a hash of `constellation:migrate:` plus the name. golang-migrate takes its own advisory lock after this lock. The keys are different, so there is no deadlock.
- `Lock` takes one connection from the pool and runs `pg_advisory_lock`. If `Lock` fails, also when the context ends, the adapter closes the connection and does not return it to the pool.
- `NewMigrator` does no database work and gets no connection.
- `Run` uses two pool connections at the same time: one for the lock and one for golang-migrate. The pool needs `MaxConns` of 2 or more. `Up` closes its connection at the end, also after an error. The pool stays open.
- The migration files are at the root of the source. A service that embeds `migrations/*.sql` passes `fs.Sub(files, "migrations")`.
- The migrator runs up migrations only. An operator runs a down migration by hand.
- If the database version is higher than the newest version in the source, `Up` applies nothing and returns `Result{Ahead: true}`.
- If the context ends during a migration, `Up` completes the current migration, applies no more, and returns the error of the context. The database is not dirty. The grace period of the orchestrator must be longer than the longest single migration.
- A dirty database stops the service. See "Dirty database" below.
- A connection pooler such as PgBouncer in transaction mode breaks session advisory locks. Migrations need a direct connection or a pool in session mode.
- `Up` uses `database/sql` and `stdlib.OpenDBFromPool`. The golang-migrate driver `database/pgx/v5` of the version in `go.mod` has no `WithConnection` function, so the migrator uses `WithInstance`. The `MaxConns` check is the control for the wait for a connection.
- golang-migrate sends each file in one call. PostgreSQL runs a call with more than one statement in one implicit transaction. A file with `CREATE INDEX CONCURRENTLY` must have only that statement.

Dirty database. A failed migration leaves the database dirty, and the next start returns `ErrDirty` with the version. The operator does these steps:

1. Read the version in the table `schema_migrations` (or the table of the migration set).
2. Fix the database by hand, so that it matches a complete migration or the version before it.
3. Run `migrate force <version>` of the golang-migrate CLI with a direct connection.
4. Start the service again.

## health

Purpose: the liveness and readiness endpoints on a separate admin port. The package is a port with the interface `Checker`, the readiness `State`, the handlers, and the admin `Server`.

Import path: `github.com/techorionai/constellation-platform-go/health`

Environment variables: nest `health.Config` with `envPrefix:"ADMIN__"`.

| Name | Type | Default | Use |
| ---- | ---- | ------- | --- |
| `<PREFIX>ADMIN__ADDR` | string | `:9090` | Optional. |
| `<PREFIX>ADMIN__CHECK_TIMEOUT` | duration | `2s` | Optional. |
| `<PREFIX>ADMIN__DRAIN_DELAY` | duration | `5s` | Optional. |

Use. The checkers are optional. A service gives only the checkers that it needs:

```go
ctx := context.Background()
var (
	db *clientpg.Client
	kv *clientvk.Client
)
pgCheck, err := healthpg.NewChecker(ctx, db.Pool)
if err != nil {
	log.Fatal(err)
}
defer pgCheck.Close()

states := []*health.State{
	// PostgreSQL only
	health.NewState(pgCheck),
	// Valkey only
	health.NewState(healthvk.NewChecker(kv.Raw)),
	// Both
	health.NewState(pgCheck, healthvk.NewChecker(kv.Raw)),
	// None, for a service that has no database
	health.NewState(),
}
for _, state := range states {
	state.MarkStarted()
}
```

A custom checker:

```go
state := health.NewState(alwaysOK{})
handler := health.NewHandler(state, health.Config{})

get := func(path string) string {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))
	body, _ := io.ReadAll(rec.Result().Body)
	return fmt.Sprintf("%d %s", rec.Code, body)
}
fmt.Print(get("/readyz"))
state.MarkStarted()
fmt.Print(get("/readyz"))
// Output:
// 503 {"type":"about:blank","title":"Service Unavailable","status":503,"detail":"The service is not ready.","code":"service_not_ready"}
// 200 {"status":"ready","checks":{"cache":"ok"}}
```

`alwaysOK` is a type with the methods `Name() string` and `Check(ctx) error`. Each such type is a checker.

The admin server and the graceful stop:

```go
ctx := context.Background()
state := health.NewState()
srv := health.NewServer(health.Config{Addr: ":9090"}, state)
go func() {
	if err := srv.ListenAndServe(); err != nil {
		log.Print(err)
	}
}()

// ... run the migrations and start the API server ...
state.MarkStarted()

// On a stop signal:
_ = srv.Drain(ctx)    // /readyz is 503 during DrainDelay
_ = srv.Shutdown(ctx) // the admin server stops last
```

Errors: `NewState` panics if a checker has an empty name or if two checkers have the same name. This is a programming error. `Server.Drain` returns the error of the context if the context ends before `DrainDelay`.

Rules and limits:

- `/livez` is `200` with `{"status":"ok"}` while the process runs.
- `/readyz` is `503` until `State.MarkStarted()`, and `503` again after `State.MarkDraining()`. After the start, it runs all checkers at the same time. Each checker gets a context with `CheckTimeout`. If all pass, the response is `200` with `{"status":"ready","checks":{...}}`. The body has only the checks in the list. A checker that has no result after `CheckTimeout` is a failed check, also if it does not obey its context.
- A `503` body is a problem document (`application/problem+json`, RFC 9457) with the same field names as the `Problem` model of `constellation-contracts`: `type`, `title`, `status`, `detail`, and `code` (`service_not_ready`). The body never contains the error text of a checker. The handler writes that text to the log at the `warn` level.
- The admin handlers make no OpenTelemetry spans, so a `503` body has no `traceId`.
- The admin server has no authentication. Do not publish the admin port at the edge. Use it for the probes of the orchestrator only.
- `DrainDelay` must be longer than the time that the load balancer needs to remove the instance. For a readiness probe, this time is the probe period multiplied by the failure threshold. It must also be longer than the time that the platform needs to remove a stopping instance from the load balancer.
  - Kubernetes: a readiness probe with `periodSeconds: 5` and `failureThreshold: 2` needs 10 seconds. Set `DrainDelay` to 15 seconds, and set `terminationGracePeriodSeconds` to more than `DrainDelay` plus the stop time of the service.
  - DigitalOcean App Platform: the health check settings of the component give the period and the failure threshold. Use the same calculation.
  - The default of 5 seconds suits a probe period of 2 seconds with a failure threshold of 2.
- The server timeouts are: read header 5 seconds, read 10 seconds, write 10 seconds or `CheckTimeout` plus 5 seconds (the longer one), idle 60 seconds.
- `health` can import `net/http`. It cannot import `pgx` or `valkey-go`.

## health/postgres

Purpose: the PostgreSQL checker of `health`.

Import path: `github.com/techorionai/constellation-platform-go/health/postgres`

Environment variables: None.

Use:

```go
ctx := context.Background()

var cfg struct {
	Database clientpg.Config `envPrefix:"DATABASE__"`
}
if err := config.Load("IDENTITY_", &cfg); err != nil {
	log.Fatal(err)
}
db, err := clientpg.New(ctx, cfg.Database)
if err != nil {
	log.Fatal(err)
}
defer db.Close()

check, err := healthpg.NewChecker(ctx, db.Pool)
if err != nil {
	log.Fatal(err)
}
defer check.Close()

state := health.NewState(check)
state.MarkStarted()
```

Errors: `NewChecker` returns an error if the pool is nil or if it cannot make the dedicated pool.

Rules and limits:

- `NewChecker` copies the config of the service pool. It sets `MaxConns` to 1, `MinConns` to 0, and the tracer to nil. It makes a dedicated pool. The check uses the same database and user as the service. A full service pool does not make the check fail, and a real PostgreSQL failure does.
- The checker uses one more connection for each instance. Include it in `max_connections` of the server.
- `Name()` is `postgres`. `Check` calls `Ping`. `Close()` closes the dedicated pool and not the service pool.

## health/valkey

Purpose: the Valkey checker of `health`.

Import path: `github.com/techorionai/constellation-platform-go/health/valkey`

Environment variables: None.

Use:

```go
ctx := context.Background()

var cfg struct {
	Valkey clientvk.Config `envPrefix:"VALKEY__"`
}
if err := config.Load("IDENTITY_", &cfg); err != nil {
	log.Fatal(err)
}
kv, err := clientvk.New(ctx, cfg.Valkey)
if err != nil {
	log.Fatal(err)
}
defer kv.Close()

state := health.NewState(healthvk.NewChecker(kv.Raw))
state.MarkStarted()
```

Errors: None. `Check` returns the error of the `PING`.

Rules and limits: `Name()` is `valkey`. `Check` sends `PING`. `valkey-go` sends commands over shared connections, so a busy client does not block the `PING`. `valkeyotel` makes a span for each `PING`. The trace sampler can drop these spans.

## executor

Purpose: give the generic executor interfaces of the hexagonal architecture guide. A command or a query implements them. A decorator implements the same interface.

Import path: `github.com/techorionai/constellation-platform-go/executor`

Environment variables: None.

Use:

```go
var create executor.Executor[CreateOrder] = executor.Func[CreateOrder](
	func(_ context.Context, p CreateOrder) error {
		fmt.Println("create", p.ID)
		return nil
	},
)
err := create.Execute(context.Background(), CreateOrder{ID: "o1"})
fmt.Println(err)
// Output:
// create o1
// <nil>
```

Errors: None. The package returns the error of the function.

Rules and limits:

- `Executor[P]` has `Execute(ctx, params) error`. `ExecutorWithReturn[P, R]` has `Execute(ctx, params) (R, error)`.
- `Func` and `FuncWithReturn` turn a function into an executor. Tests and small use cases use them.

## decorators

Purpose: add tracing and logging to an executor.

Import path: `github.com/techorionai/constellation-platform-go/decorators`

Environment variables: None.

Use:

```go
var cmd executor.ExecutorWithReturn[CreateOrder, string] = executor.FuncWithReturn[CreateOrder, string](
	func(_ context.Context, p CreateOrder) (string, error) { return "created " + p.ID, nil },
)
// The recommended order, innermost first: the command, logging, tracing.
cmd = decorators.ApplyLogging(cmd, "CreateOrder")
cmd = decorators.ApplyTracing(cmd, "CreateOrder")

res, err := cmd.Execute(context.Background(), CreateOrder{ID: "o1"})
fmt.Println(res, err)
// Output: created o1 <nil>
```

Use `WithErrorClassifier` for expected errors:

```go
errValidation := errors.New("validation")
cmd := executor.FuncWithReturn[int, int](func(context.Context, int) (int, error) { return 0, errValidation })
// A validation error is expected. The span keeps the status Unset.
traced := decorators.ApplyTracing[int, int](cmd, "Validate",
	decorators.WithErrorClassifier(func(err error) bool { return !errors.Is(err, errValidation) }))
_, err := traced.Execute(context.Background(), 1)
fmt.Println(err)
// Output: validation
```

Errors: a decorator returns the error of the wrapped executor without a change, so `errors.Is` and `errors.As` work. An invalid name causes a panic when you make the decorator.

Rules and limits:

- Functions: `ApplyTracing`, `ApplyLogging`, `ApplyTracingNoResult`, and `ApplyLoggingNoResult`. The `NoResult` forms wrap `executor.Executor`.
- The name matches `^[A-Za-z][A-Za-z0-9_.]{0,63}$`.
- `ApplyTracing` makes the span `executor:<name>` with the attribute `executor.name`, and records `executor.duration`. See the metrics table in [conventions.md](conventions.md#metrics).
- `WithErrorClassifier(func(error) bool)`: return false for an expected error, for example a validation error or "not found". The span records the error event and keeps the status `Unset`, and `outcome` is `expected_error`. Without this option, each error is a failure.
- `ApplyLogging` writes a `debug` record `executor completed` on success and a record `executor failed` on an error. The level of the second record is `error`. `WithErrorLevel(func(error) slog.Level)` changes it, for example to `info` for a validation error.
- A decorator never logs the params or the result, because they can contain secrets and personal data.
- Options: `WithTracerProvider`, `WithMeterProvider`, `WithLogger`, `WithErrorLevel`, and `WithErrorClassifier`. The defaults are the global providers and `slog.Default()`.
- The decorator reads the global providers and `slog.Default()` when it is made, and it keeps them. Make the decorators after `telemetry.Setup`. A decorator that is made before `telemetry.Setup` keeps the old logger and the old providers.
- Order, innermost first: the command, then logging, then tracing. Tracing is the outermost decorator, so the log records have the `trace_id` of the executor span.

## cache

Purpose: the cache port and the typed cache. The adapter is in [`cache/valkey`](#cachevalkey).

Import path: `github.com/techorionai/constellation-platform-go/cache`

Environment variables: None.

Use:

```go
// A service uses the Valkey adapter. This sample uses the mock store.
var store cache.Store = &mock.CacheStore{}
profiles := cache.New[Profile](store, "profile")

load := func(context.Context) (Profile, error) {
	fmt.Println("load")
	return Profile{Name: "Ada"}, nil
}
for range 2 {
	p, err := profiles.GetOrLoad(context.Background(), "user:42", time.Minute, load)
	fmt.Println(p.Name, err)
}
// Output:
// load
// Ada <nil>
// Ada <nil>
```

`Instrument` records the store metrics for code that uses a `Store` without `Cache[T]`:

```go
store := cache.Instrument(&mock.CacheStore{}, "raw")
err := store.Set(context.Background(), "k", []byte("v"), time.Minute)
fmt.Println(err)
// Output: <nil>
```

Errors:

- `ErrMiss`: the key does not exist.
- `ErrInvalidKey`: the key is empty or longer than 256 bytes.
- `ErrInvalidTTL`: the TTL is shorter than 1 millisecond or longer than 30 days.
- `ErrTooLarge`: the value is longer than 1 MiB.
- `ErrDecode`: a stored value does not decode.
- `ErrLoadPanic`: the `load` function of `GetOrLoad` panicked. `GetOrLoad` recovers the panic, writes an `error` log record with the type of the panic value, and returns this error to all callers that wait for the same load. The error text does not contain the panic value.

Rules and limits:

- `Store` has `Get`, `Set`, and `Delete`. `cache.CheckKey`, `cache.CheckTTL`, and `cache.CheckValue` do the checks. Each adapter calls them before it sends a command.
- The namespace of `New` matches `^[a-z][a-z0-9_]{0,31}$`. An invalid namespace causes a panic. The store key is `<namespace>:<key>`, so the namespace counts in the limit of 256 bytes.
- A cache entry always has a TTL. Use a short TTL for data that changes.
- The default codec is JSON. `WithCodec` changes it. The JSON codec writes a `config.Secret` field as `"[redacted]"` without an error. Do not put secrets in the cache.
- `GetOrLoad` checks the key and the TTL first. It returns `ErrInvalidKey` or `ErrInvalidTTL` without a call of `load`.
- A miss, a decode error, and a store error all lead to a call of `load`. A store error or a decode error gives a `warn` log record. The cache is not the source of truth.
- Calls for the same key in one process at the same time share one `load` call and one `Set`. Other instances can also load. `load` gets the context of the first caller without its cancel, with a time limit. `WithLoadTimeout(d)` sets it. The default is 10 seconds. If the context of a caller ends, that caller returns the context error and `load` continues for the other callers.
- If `load` returns an error, `GetOrLoad` returns it and stores nothing.
- Each method makes a span `cache.<method>` with the attributes `cache.namespace` and `cache.hit`. A span, a log record, or a metric attribute never contains a key or a value.
- Metrics: see [conventions.md](conventions.md#metrics).
- In `WithinTx`, call `Set` and `Delete` through `AfterCommit`. Do not call `GetOrLoad` in a `WithinTx` that changes the data that `load` reads.

## cache/valkey

Purpose: the Valkey adapter of `cache.Store`.

Import path: `github.com/techorionai/constellation-platform-go/cache/valkey`

Environment variables: None. The adapter uses the client of [`clients/valkey`](#clientsvalkey).

Use:

```go
ctx := context.Background()

var cfg struct {
	Valkey clientvk.Config `envPrefix:"VALKEY__"`
}
if err := config.Load("IDENTITY_", &cfg); err != nil {
	log.Fatal(err)
}
kv, err := clientvk.New(ctx, cfg.Valkey)
if err != nil {
	log.Fatal(err)
}
defer kv.Close()

profiles := cache.New[Profile](cachevk.NewStore(kv), "profile")
p, err := profiles.GetOrLoad(ctx, "user:42", time.Minute,
	func(context.Context) (Profile, error) { return Profile{Name: "Ada"}, nil })
if err != nil {
	log.Fatal(err)
}
log.Print(p.Name)
```

Errors: the errors of `cache`. `Get` maps a nil reply to `cache.ErrMiss`. It wraps each other error, for example `NOPERM`, and that error is not `ErrMiss`.

Rules and limits:

- The adapter makes each key with `client.Keys.Key("cache", key)`. Example: the cache key `user:42` in namespace `profile` is `identity:cache:profile:user:42` for the key prefix `identity`. The ACL rule `~identity:*` covers it.
- Commands: `GET`, `SET key value PX <ms>`, and `DEL`. `Set` rounds the TTL up to whole milliseconds.
- `Delete` sends one `DEL` for each key in one `DoMulti` call. It works in standalone mode and in cluster mode, where the keys can be in different slots. `Delete` with no keys sends no command. If one or more `DEL` commands fail, it returns the errors joined.
- The tests use a standalone Valkey. They do not use a Valkey cluster.

## transaction

Purpose: the transaction port. The adapter is in [`transaction/postgres`](#transactionpostgres).

Import path: `github.com/techorionai/constellation-platform-go/transaction`

Environment variables: None.

Use:

```go
// A service uses the adapter of transaction/postgres. This sample uses the
// mock, which follows the same rules for the hooks.
var tx transaction.Transactor = &mock.Transactor{}
ctx := context.Background()

err := tx.WithinTx(ctx, func(ctx context.Context) error {
	tx.AfterCommit(ctx, func(context.Context) { fmt.Println("after commit") })
	fmt.Println("in transaction")
	return nil
}, transaction.Name("create_order"))
fmt.Println(err)
// Output:
// in transaction
// after commit
// <nil>
```

Errors:

- `ErrSerialization`: the database stopped the transaction because of a conflict. The caller can try again. The package does not retry.
- `ErrNestedOptions`: a nested `WithinTx` call has `ReadOnly` or `Isolation`.
- `ErrInvalidOption`: the name does not match `^[a-z][a-z0-9_]{0,63}$`, or the isolation level is not defined. `WithinTx` returns it before it starts the transaction and before it calls `fn`.
- `ErrForeignTx`: the context has a transaction that the transaction adapter did not start.

Rules and limits:

- Options: `ReadOnly()`, `Isolation(level)`, and `Name(name)`. The levels are `ReadCommitted` (default), `RepeatableRead`, and `Serializable`. `Name` gives the transaction a name for the span and the metrics. The default is `unnamed`. The names are fixed in the code.
- `Resolve(opts...)` checks the options and returns the settings. The adapter and `mock.Transactor` call it. `Settings.HasTxOptions()` tells if the options include `ReadOnly` or `Isolation`.
- `WithinTx` commits if `fn` returns nil. It rolls back if `fn` returns an error or panics.
- If the commit returns an error, the outcome is unknown. The server can have committed the transaction when the connection broke or the time limit ended. `WithinTx` returns an error and does not run the `AfterCommit` hooks, so a cache entry that a hook would delete stays until its TTL ends. Do not assume that the work did not happen. Make the work idempotent, or read the data again before a retry.
- A nested `WithinTx` joins the outer transaction. It can set `Name` only. There are no savepoints, so an inner error that the outer function ignores does not roll back the inner work alone.
- `AfterCommit` rules:
  - The hooks run in the order of registration after the commit of the outermost transaction. They do not run if the transaction rolls back or the commit fails.
  - A hook gets a context without the cancel of the caller, with a time limit. A hook must obey its context.
  - A hook does not return an error. A panic in a hook is recovered and logged, and the next hooks still run.
  - Outside a transaction, `AfterCommit` runs the hook at once.
- The package must not import a database driver or `net/http`. The `depguard` rules enforce it.

## transaction/postgres

Purpose: the PostgreSQL adapter of `transaction.Transactor`.

Import path: `github.com/techorionai/constellation-platform-go/transaction/postgres`

Environment variables: None. The adapter uses the pool of [`clients/postgres`](#clientspostgres).

Use:

```go
ctx := context.Background()

var cfg struct {
	Database clientpg.Config `envPrefix:"DATABASE__"`
}
if err := config.Load("IDENTITY_", &cfg); err != nil {
	log.Fatal(err)
}
db, err := clientpg.New(ctx, cfg.Database)
if err != nil {
	log.Fatal(err)
}
defer db.Close()

tx := txpg.NewTransactor(db.Pool)
err = tx.WithinTx(ctx, func(ctx context.Context) error {
	// A repository gets its connection with Conn for each call. It joins
	// this transaction.
	_, err := clientpg.Conn(ctx, db.Pool).Exec(ctx, "INSERT INTO items (id) VALUES ($1)", "i1")
	if err != nil {
		return err
	}
	tx.AfterCommit(ctx, func(context.Context) { log.Print("committed") })
	return nil
}, transaction.Name("create_item"))
if err != nil {
	log.Fatal(err)
}
```

Errors: the errors of `transaction`. SQLSTATE `40001` and `40P01` give an error that wraps `transaction.ErrSerialization` and the original error. If `fn` returns an error and the rollback also fails, `WithinTx` returns both errors joined.

Rules and limits:

- `NewTransactor(pool, opts...)`. `WithHookTimeout(d)` sets the time limit of each `AfterCommit` hook. The default is 5 seconds. A value of 0 or less causes a panic.
- `WithinTx` starts a transaction with the options, calls `fn` with a context that has the transaction, and commits or rolls back. A panic in `fn` rolls back, then the same panic goes on. If `fn` calls `runtime.Goexit` (for example through `testing.T.FailNow`), `WithinTx` rolls back, releases the connection, and records the outcome `goexit`.
- The commit and the rollback use a context that keeps the values of the caller context but not its cancel. The commit has a time limit of 30 seconds and the rollback has a time limit of 10 seconds. A cancel of the caller context after `fn` returns nil does not stop the commit.
- If a hook does not end in the time limit, the adapter writes an `error` log record and goes on with the next hook. The goroutine of that hook ends later. `WithinTx` returns after all hooks end or reach the time limit.
- The hook list is safe for concurrent use. Goroutines that `fn` starts can call `AfterCommit`.
- The hook list closes when the outermost transaction ends. A call of `AfterCommit` after that drops the hook and writes an `error` log record.
- Only the transaction adapter calls `clients/postgres.ContextWithTx`. If the context has a transaction but no hook list of the adapter, `AfterCommit` panics and `WithinTx` returns `ErrForeignTx`.
- The adapter makes the span `db.transaction` with the attributes `db.transaction.name`, `db.transaction.isolation`, and `db.transaction.read_only`. The span has the `outcome` and no error text.
- Metrics: see [conventions.md](conventions.md#metrics).

## secrets

Purpose: store secret values (LLM API keys, SMTP credentials, webhook signing keys, OAuth client secrets) with envelope encryption. `Seal` and `Open` do the encryption. `Service` checks the names and the sizes and stores the envelopes through the `Repository` port. `KeyRing` holds the master keys.

Import path: `github.com/techorionai/constellation-platform-go/secrets`

Environment variables (with the prefix of the service and `SECRETS__`, for example `IDENTITY_SECRETS__MASTER_KEY`):

| Variable | Required | Meaning |
| -------- | -------- | ------- |
| `MASTER_KEY` or `MASTER_KEY_FILE` | Yes | The current master key: 32 bytes, base64 (standard encoding). |
| `MASTER_KEY_ID` | Yes | The ID of the current key. It matches `^[a-z0-9-]{1,32}$`. |
| `PREVIOUS_KEYS` or `PREVIOUS_KEYS_FILE` | No | Keys for rotation. The format is `<id>:<base64>` items, separated by commas. |

Make a master key:

```sh
openssl rand -base64 32
```

Use:

```go
ctx := context.Background()
// A service uses the PostgreSQL repository of secrets/postgres. The mock
// keeps the records in memory.
svc := secrets.NewService(exampleRing(), &mock.SecretsRepository{})

if err := svc.Put(ctx, "llm/api-key", []byte("value")); err != nil {
	fmt.Println(err)
	return
}
value, err := svc.Get(ctx, "llm/api-key")
fmt.Println(string(value), err)

names, err := svc.List(ctx, secrets.ListParams{})
fmt.Println(names.Names, err)

_, err = svc.Get(ctx, "missing")
fmt.Println(err)
// Output:
// value <nil>
// [llm/api-key] <nil>
// secrets: not found
```

`exampleRing` is a helper of the example. A service makes the ring with `secrets.NewKeyRing(cfg.Secrets)`, where `cfg.Secrets` is a `secrets.Config` with the tag `envPrefix:"SECRETS__"`. The sample is `ExampleService` in `example_test.go`.

Errors:

- `ErrInvalidConfig` (from `NewKeyRing`): a key is not valid base64 or does not have 32 bytes, an ID does not match the rule, an ID is used two times, the current ID is in the list of previous keys, or a previous key item has no `:`. The text never contains a key.
- `ErrInvalidName`, `ErrEmptyValue`, `ErrTooLarge`, `ErrInvalidLimit`, `ErrInvalidBatchSize`: the input is not valid.
- `ErrNotFound`: no secret has the name.
- `ErrDecrypt`: the authentication failed. The stored data is changed, copied from another row, or sealed with another key. `ErrUnknownKey`: the ring has no key with the key ID of the record. `ErrUnsupportedFormat`: the envelope format is not 1.
- `ErrConflict` (from the repository): the stored envelope changed. `Rewrap` counts it and does not return it.

Rules and limits:

- A name matches `^[a-z0-9][a-z0-9._:/-]{0,199}$`. A value has 1 byte to 64 KiB. `List` gives names only, 100 by default and 1000 at most. It uses the last name of a page as the cursor.
- No span, log record, metric attribute, or error contains a value or a key. A span has the attribute `secret.name`.
- The data ciphertext binds to the name. A ciphertext that is copied to another row does not decrypt.
- The calling service checks the permissions and writes the audit entries.
- A backup holds the encrypted secrets and not the master key. A backup without the master key cannot be decrypted, and a lost master key makes all secrets lost. Keep the master key in a different place from the backup.
- A random 96-bit nonce is safe for far more than the number of `Put` and `Rewrap` calls of a self-hosted install, because each secret has its own data key.

Rotate the master key. A service can run more than one instance, and old and new instances run at the same time during a rolling restart. Thus the rotation uses three deployments:

1. Add the new key `k2` to `PREVIOUS_KEYS` on all instances. `k1` stays current. All instances can now open `k2` envelopes. No instance writes with `k2`.
2. Make `k2` current and move `k1` to `PREVIOUS_KEYS`. During the restart, old instances write with `k1` and new instances write with `k2`. Both can open both.
3. Run `Service.Rewrap` until `Conflicts` and `Failed` are 0. Then delete `k1` from `PREVIOUS_KEYS`.

`Rewrap(ctx, batchSize)` takes a `batchSize` from 1 to 1000 and returns `RewrapResult{Rewrapped, Conflicts, Failed}`. A record that changed during the rewrap counts in `Conflicts`. Run `Rewrap` again. A record that no key of the ring can open counts in `Failed`, and `Rewrap` writes an `error` log record with its name and the kind of the error. Examine each failed record, and do not delete the old key until `Failed` is 0. A service gives its own entry point for `Rewrap`. See [Admin sample](#admin-sample-rotation-and-a-manual-retention-run).

## secrets/postgres

Purpose: the PostgreSQL adapter of `secrets.Repository`, with its embedded migration for the table `platform_secrets`.

Import path: `github.com/techorionai/constellation-platform-go/secrets/postgres`

Environment variables: None.

Use:

```go
ctx := context.Background()

var cfg struct {
	Database clientpg.Config `envPrefix:"DATABASE__"`
	Secrets  secrets.Config  `envPrefix:"SECRETS__"`
}
if err := config.Load("IDENTITY_", &cfg); err != nil {
	log.Fatal(err)
}
db, err := clientpg.New(ctx, cfg.Database)
if err != nil {
	log.Fatal(err)
}
defer db.Close()

// Run the secrets migrations with their own migrations table. The pool
// needs MaxConns of 2 or more.
migrator, err := pgmigrate.NewMigrator(db.Pool, secretspg.Migrations(),
	pgmigrate.MigratorOptions{MigrationsTable: secretspg.MigrationsTable})
if err != nil {
	log.Fatal(err)
}
_, err = migrate.Run(ctx, pgmigrate.NewLocker(db.Pool, secretspg.MigrationsTable), migrator,
	migrate.Options{Name: secretspg.MigrationsTable})
if err != nil {
	log.Fatal(err)
}

ring, err := secrets.NewKeyRing(cfg.Secrets)
if err != nil {
	log.Fatal(err)
}
svc := secrets.NewService(ring, secretspg.NewRepository(db.Pool))
if err := svc.Put(ctx, "llm/api-key", []byte("value")); err != nil {
	log.Fatal(err)
}
```

The sample is `ExampleNewRepository` in `example_test.go`.

Errors:

- The repository returns `secrets.ErrNotFound` for `Get` and `Delete` of an unknown name, and `secrets.ErrConflict` for `UpdateEnvelope` when the stored envelope is not the expected one. Other errors wrap the error of the driver. They contain no value, because the table holds ciphertext only.

Rules and limits:

- A service runs `migrate.Run` for its own migrations, and one more time for these migrations, with a separate `Migrator` and `Locker`. The lock name, the migrations table, and `Options.Name` are all `secretspg.MigrationsTable` (`platform_secrets_schema_migrations`). The two sets have different lock keys, and `migrate.duration` shows each set with `migrate.name`. The two sets do not share version numbers.
- `Migrations()` returns the embedded files at the root, as `migrate/postgres.NewMigrator` needs.
- The table has the columns `name` (primary key), `format`, `key_id`, `wrapped_dek`, `nonce`, `ciphertext`, `created_at`, and `updated_at`, and an index on `key_id`.
- Each method gets its connection with `clients/postgres.Conn(ctx, pool)`. Thus `Service.Put` joins the transaction of the caller, if there is one.
- `Upsert` replaces the envelope of an existing name and sets `updated_at`. `UpdateEnvelope` changes the row only if `key_id` and `wrapped_dek` are still the old values.
- A later change to `platform_secrets` follows the expand and contract rule.

## retention

Purpose: delete expired data with background jobs. A `Job` has a name, a `Config`, a default period, and a `Purger`. The `Runner` runs each job in its own goroutine. A `Locker` makes sure that only one instance runs a job at a time.

Import path: `github.com/techorionai/constellation-platform-go/retention`

Environment variables: one set for each job. A service nests one `retention.Config` for each job with `envPrefix:"RETENTION__<JOB>__"`. `<JOB>` is the job name in upper case.

| Variable | Default | Meaning |
| -------- | ------- | ------- |
| `<PREFIX>RETENTION__<JOB>__PERIOD` | None | The age after which data expires, for example `1440h`. Optional. |
| `<PREFIX>RETENTION__<JOB>__INTERVAL` | `1h` | The time between the end of a tick and the start of the next tick. 1 minute or more. |
| `<PREFIX>RETENTION__<JOB>__BATCH_SIZE` | `1000` | The maximum number of items that one `Purge` call deletes. From 1 to 100000. |

Example: `IDENTITY_RETENTION__AUDIT__PERIOD=1440h`.

Use:

```go
runner, err := retention.NewRunner(sampleLocker{}, []retention.Job{{
	Name:          "audit",
	Config:        retention.Config{Interval: time.Hour, BatchSize: 100},
	DefaultPeriod: 60 * 24 * time.Hour,
	Purger:        &samplePurger{left: 250},
}}, retention.RunnerOptions{})
if err != nil {
	fmt.Println(err)
	return
}
n, err := runner.RunOnce(context.Background(), "audit")
fmt.Println(n, err)
// Output: 250 <nil>
```

`sampleLocker` and `samplePurger` do no database work. A service uses the adapters of [retention/postgres](#retentionpostgres). The sample is `ExampleRunner_RunOnce` in `example_test.go`.

To add a retention job: put a `retention.Config` in the config struct with the `envPrefix` of the job, set `DefaultPeriod` in code from `VISION.md` section 15, make a `Purger`, add a `Job` to the runner, and start `Run` after the migrations. The period of a job is `Config.Period` if it is more than 0. Else it is `DefaultPeriod`.

Errors:

- `NewRunner` returns `ErrInvalidJob` (wrong name, nil purger, or nil locker), `ErrInvalidConfig` (from `Config.Validate`), `ErrNoPeriod` (`Period` and `DefaultPeriod` are both 0, or `DefaultPeriod` is negative), and `ErrDuplicateJob`.
- `RunOnce` returns `ErrUnknownJob` for an unknown name. It returns `0, ErrLocked` when another instance has the lock, so an admin tool can tell the operator that the job did not run. `0, nil` means that the job ran and found no expired items.

Rules and limits:

- `Validate` checks `Period >= 0`, `Interval >= 1m`, and `1 <= BatchSize <= 100000`. A `Config` made in code must set `Interval` and `BatchSize`. The defaults come from the environment tags.
- `Run` starts one goroutine for each job and returns nil when the context ends. A job waits a random time from 0 to `Interval / 10`, runs its first tick, and then runs the next tick `Interval` after the end of the previous tick. The jobs are independent. A tick with an error does not stop `Run`. `Run` treats `ErrLocked` as a skipped tick and writes no `error` log record for it.
- A tick takes the lock `retention:<name>`, calls `Purge` with `BatchSize` until it returns less than `BatchSize`, and releases the lock with a new context of 10 seconds, also after an error.
- Each tick makes the span `retention.tick` and records the metrics `retention.deleted`, `retention.duration`, and `retention.last_success`. See [conventions.md](conventions.md#retention-metrics) for the outcome values and the alert rules. A normal stop gives the outcome `canceled` and an `info` log record, not an `error` outcome.
- `RunnerOptions.Clock` and `RunnerOptions.After` are for tests. A test gives an `After` function that returns a channel that the test controls.

## retention/postgres

Purpose: the PostgreSQL adapters of the retention ports. `TablePurger` deletes the expired rows of one table in batches. `Locker` is a session advisory lock that does not wait.

Import path: `github.com/techorionai/constellation-platform-go/retention/postgres`

Environment variables: None.

Use:

```go
ctx := context.Background()

var cfg struct {
	Database       clientpg.Config  `envPrefix:"DATABASE__"`
	AuditRetention retention.Config `envPrefix:"RETENTION__AUDIT__"`
}
if err := config.Load("IDENTITY_", &cfg); err != nil {
	log.Fatal(err)
}
// The pool needs MaxConns of 2 or more. Each tick of a job uses 2
// connections, so leave spare connections for the jobs.
db, err := clientpg.New(ctx, cfg.Database)
if err != nil {
	log.Fatal(err)
}
defer db.Close()

purger, err := retpg.NewTablePurger(db.Pool, retpg.TableSpec{Table: "audit_entries", TimeColumn: "created_at"})
if err != nil {
	log.Fatal(err)
}
locker, err := retpg.NewLocker(db.Pool)
if err != nil {
	log.Fatal(err)
}
runner, err := retention.NewRunner(locker, []retention.Job{{
	Name:          "audit",
	Config:        cfg.AuditRetention,
	DefaultPeriod: 60 * 24 * time.Hour,
	Purger:        purger,
}}, retention.RunnerOptions{})
if err != nil {
	log.Fatal(err)
}
// Run stops and returns when ctx ends.
go func() { _ = runner.Run(ctx) }()
```

The sample is `ExampleNewTablePurger` in `example_test.go`.

Errors:

- `ErrPoolTooSmall` (from `NewLocker`): the pool has `MaxConns` below 2.
- `NewTablePurger` returns an error if a name in `TableSpec` does not match `^[a-z_][a-z0-9_]{0,62}$`, and it runs no SQL.

Rules and limits:

- `TableSpec` has `Schema` (default `public`), `Table`, `TimeColumn`, and `KeyColumn` (default `id`). The adapter quotes the names with `pgx.Identifier`.
- The statement is `DELETE FROM <t> WHERE <key> IN (SELECT <key> FROM <t> WHERE <col> < $1 ORDER BY <col> LIMIT $2) AND <col> < $1`. It uses a key column and not `ctid`, because a `ctid` is unique in one partition only. The statement works on normal tables and on partitioned tables. The outer time condition keeps the rows that are not expired, also when the key is unique only together with the partition key.
- The time column has the type `timestamptz`. The key column is the primary key, or a part of the primary key of a partitioned table. Add an index on the time column.
- A key column that is not unique can delete more rows in one batch than `BatchSize`. The outer time condition keeps all rows that are not expired.
- `Locker` key: the 64-bit FNV-1a hash of `constellation:` plus the name. `TryLock` takes one connection and runs `pg_try_advisory_lock`. If another session has the lock, it releases the connection. The connection stays with the lock until `unlock`. This adapter does not import `migrate/postgres`. It has its own small lock code.
- A tick uses two pool connections at the same time: the lock connection for the full tick, and one connection for each `Purge` call. N jobs whose ticks run at the same time can use 2N connections. Set `MaxConns` with spare connections for the jobs, in addition to the connections for requests.
- The adapters use the pool directly. A retention tick is not part of a caller transaction.
- A connection pooler such as PgBouncer in transaction mode breaks session advisory locks. Retention jobs need a direct connection or a pool in session mode.

## requestid

Purpose: give a request ID to each HTTP request, and put it on the response, the span, and the log records.

Import path: `github.com/techorionai/constellation-platform-go/requestid`

Environment variables: None.

Use:

```go
handler := requestid.Middleware()(mux)
```

In a handler, `requestid.FromContext(ctx)` returns the ID.

Errors: None.

Rules and limits:

- The middleware keeps the header `X-Request-ID` of the request if it matches `^[A-Za-z0-9._-]{1,128}$`. Otherwise it makes a new UUID v7.
- The ID goes in the context, in the response header `X-Request-ID`, on the active span as `http.request.id`, and on the log records as `request.id`.
- A log record gets `request.id` only if the code writes it with the request context, for example with `slog.InfoContext(ctx, ...)`. The log handler of `telemetry` adds the fields. See `telemetry.ContextWithAttrs`.
- The request ID is not a security value. A client can send any ID that matches the pattern. Do not use it for an access decision.
- Put this middleware first, so that the error bodies of `auth` have the `requestId`.

## auth

Purpose: the principal model, the token verification port, the scope checks, and the `net/http` middleware. The package is a port. The adapter is `auth/jwt`.

Import path: `github.com/techorionai/constellation-platform-go/auth`

Environment variables: None. See `auth/jwt` for the verifier settings.

Use. The order of the middleware is `requestid`, then `auth.Middleware`, then `auth.RequireScopes` for each route:

```go
var verifier auth.Verifier // for example the verifier of auth/jwt

read := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	_, _ = fmt.Fprintf(w, "hello %s", p.ID)
})
mux := http.NewServeMux()
mux.Handle("GET /tickets", auth.RequireScopes("demo:ticket:read")(read))
handler := requestid.Middleware()(auth.Middleware(verifier)(mux))
```

Types:

- `Principal` has `ID`, `Type` (`user`, `agent`, `service`, or `workflow`), `ClientID`, `Scopes`, `Audience`, and `Act` (the acting principal, or nil).
- `Verifier` is the port: `Verify(ctx, token) (Principal, error)`.
- `Principal.HasScopes` and `Principal.MissingScopes` check the scopes.

Errors. `Verify` wraps one of these errors, so `errors.Is` works. The middleware maps them to an HTTP response. The body is `application/problem+json` with the fields `type`, `title`, `status`, `code`, `requestId`, and `traceId` (if a span is active). `detail` never contains the error text of `Verify`.

| Error | HTTP status | `WWW-Authenticate` | Problem `code` | Problem `title` | Metric `outcome` |
| ----- | ----------- | ------------------ | -------------- | --------------- | ---------------- |
| `ErrNoToken` | 401 | `Bearer` | `unauthorized` | `Authentication required` | `no_token` |
| `ErrInvalidToken` | 401 | `Bearer error="invalid_token"` | `unauthorized` | `Authentication required` | `invalid` |
| `ErrExpired` | 401 | `Bearer error="invalid_token"` | `unauthorized` | `Authentication required` | `expired` |
| `ErrWrongIssuer` | 401 | `Bearer error="invalid_token"` | `unauthorized` | `Authentication required` | `wrong_issuer` |
| `ErrWrongAudience` | 401 | `Bearer error="invalid_token"` | `unauthorized` | `Authentication required` | `wrong_audience` |
| `ErrKeysUnavailable` | 503, `Retry-After: 5` | none | `auth_unavailable` | `Authentication unavailable` | `keys_unavailable` |
| Other error | 401 | `Bearer error="invalid_token"` | `unauthorized` | `Authentication required` | `invalid` |
| Missing scope (from `RequireScopes`) | 403 | `Bearer error="insufficient_scope", scope="..."` | `insufficient_scope` | `Missing scopes` | Not counted |

A 403 body also has `missingScopes`, the list of the scopes that the principal lacks.

Rules and limits:

- The scheme `Bearer` is case-insensitive. A request with no header, another scheme, or an empty token gets `ErrNoToken`.
- The middleware counts each verification in the metric `constellation.auth.verify`. The adapter does not count it, so one request is one count.
- On success, the span gets `enduser.id`, `constellation.principal.type`, and `constellation.client.id`. The log records get `principal.id`, `principal.type`, and `client.id`.
- `RequireScopes` fails closed. If the context has no principal (the chain has no `auth.Middleware`), it returns a 500 problem with the code `auth_not_configured`, writes one `error` log record, and does not call the next handler.
- The token is never in a log record, a span, a metric, or an error body.
- The principal comes only from the verified token. Never read it from a header or from baggage.
- Each service checks the audience and the scopes of each request.

## auth/jwt

Purpose: the adapter of `auth.Verifier` for RFC 9068 access tokens. It verifies the tokens locally with the JWKS of the identity service.

Import path: `github.com/techorionai/constellation-platform-go/auth/jwt`

Environment variables: nest `jwt.Config` with `envPrefix:"AUTH__"`.

| Name | Type | Default | Use |
| ---- | ---- | ------- | --- |
| `<PREFIX>AUTH__ISSUER` | string | none | Required. The `iss` claim that the verifier requires. |
| `<PREFIX>AUTH__AUDIENCE` | string | none | Required. The `AppKey` of this service. The `aud` claim must contain it. |
| `<PREFIX>AUTH__JWKS_URL` | string | `<Issuer>/.well-known/jwks.json` | Optional. |
| `<PREFIX>AUTH__REFRESH_INTERVAL` | duration | `10m` | Optional. The time between the background refreshes. |
| `<PREFIX>AUTH__MIN_REFETCH_INTERVAL` | duration | `30s` | Optional. The shortest time between two fetches that an unknown key ID starts. |
| `<PREFIX>AUTH__LEEWAY` | duration | `30s` | Optional. The clock skew that the time claims allow. |
| `<PREFIX>AUTH__HTTP_TIMEOUT` | duration | `5s` | Optional. The time limit of one JWKS fetch. |

Use:

```go
var cfg struct {
	Auth jwt.Config `envPrefix:"AUTH__"`
}
if err := config.Load("DEMO_", &cfg); err != nil {
	log.Fatal(err)
}
verifier, err := jwt.New(cfg.Auth)
if err != nil {
	log.Fatal(err)
}
verifier.Start(ctx) // optional background refresh
defer verifier.Close()
handler := auth.Middleware(verifier)(mux)
```

Errors: `New` returns an error that names the missing variable. `Verify` returns the errors of the `auth` section.

Rules and limits:

- The checks run in this order: compact JWS, `alg` is `ES256` (all other values, also `none`, fail), `typ` is `at+jwt` or `application/at+jwt` (case-insensitive), `kid` is in the key set, signature, `iss`, `aud` (a string or an array), `exp` (required, with `Leeway`), `nbf` and `iat` (with `Leeway`), `sub`, `client_id`, and `principal_type`, each `scope` key matches the `PermissionKey` pattern, and `act`.
- A key set accepts a key only if `kty` is `EC`, `crv` is `P-256`, `use` is `sig` or absent, `alg` is `ES256` or absent, `kid` is set, and the key has no private part. Other keys are ignored. A key set with no accepted key is a failed fetch.
- The first `Verify` fetches the JWKS. A service starts when identity is down. Until a fetch works, `Verify` returns `ErrKeysUnavailable`.
- A refresh replaces the full key set. A key that the issuer removed stops working after the next refresh.
- A failed fetch keeps the old keys. A fetch fails on a status other than 200, on a body that is not valid JSON, on a body larger than 64 KiB, and on a key set with no accepted key.
- A token with an unknown `kid` starts one refetch, at most once in `MinRefetchInterval`. Concurrent calls share one fetch.
- `Start` is optional. Without it, the cache refreshes only when a token has an unknown `kid`. `Close` stops the refresh and waits for it. A second `Close` does nothing. `Verify` after `Close` still uses the cached keys.
- Metric `constellation.auth.jwks.fetch` (counter, unit `{fetch}`) has the attribute `outcome` with the values `ok`, `http_error`, `bad_body`, `too_large`, `no_keys`, and `network_error`.
- Opaque API tokens, introspection, and revocation are not part of this package.

## Admin sample: rotation and a manual retention run

```go
ctx := context.Background()
var (
	svc    *secrets.Service
	runner *retention.Runner
)

// Rotation, step 3: rewrap until Conflicts and Failed are 0.
res, err := svc.Rewrap(ctx, 100)
fmt.Printf("rewrapped=%d conflicts=%d failed=%d\n", res.Rewrapped, res.Conflicts, res.Failed)
if err != nil {
	log.Fatal(err)
}
if res.Conflicts > 0 || res.Failed > 0 {
	fmt.Println("run the command again, and examine the failed records before you remove the old key")
}

// A manual run of one retention job.
n, err := runner.RunOnce(ctx, "audit")
switch {
case errors.Is(err, retention.ErrLocked):
	fmt.Println("another instance runs the job now, so this call did nothing")
case err != nil:
	log.Fatal(err)
default:
	fmt.Printf("deleted %d rows\n", n)
}
```

The sample is `Example_admin` in `example_admin_test.go`. A service gives its own entry point for these calls, for example an admin subcommand.

## Full sample: a command with a transaction, the outbox, and a cache

```go
ctx := context.Background()
var (
	db *postgres.Client
	kv *valkey.Client
)

repo := orderRepo{pool: db.Pool}
tx := txpg.NewTransactor(db.Pool)
orders := cache.New[order](cachevk.NewStore(kv), "order")

rename := executor.Func[renameOrder](func(ctx context.Context, p renameOrder) error {
	return tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := repo.rename(ctx, p.ID, p.Name); err != nil {
			return err
		}
		if err := repo.writeOutbox(ctx, p.ID); err != nil {
			return err
		}
		// Delete the cache entry after the commit. Then another request
		// cannot store the old value again before the commit.
		tx.AfterCommit(ctx, func(ctx context.Context) { _ = orders.Delete(ctx, p.ID) })
		return nil
	}, transaction.Name("rename_order"))
})

// Innermost first: the command, then logging, then tracing.
var cmd executor.Executor[renameOrder] = rename
cmd = decorators.ApplyLoggingNoResult(cmd, "RenameOrder")
cmd = decorators.ApplyTracingNoResult(cmd, "RenameOrder")

if err := cmd.Execute(ctx, renameOrder{ID: "o1", Name: "New name"}); err != nil {
	log.Fatal(err)
}

// A read path uses GetOrLoad.
o, err := orders.GetOrLoad(ctx, "o1", time.Minute, func(ctx context.Context) (order, error) {
	var out order
	err := postgres.Conn(ctx, db.Pool).QueryRow(ctx, `SELECT id, name FROM orders WHERE id = $1`, "o1").Scan(&out.ID, &out.Name)
	return out, err
})
if err != nil {
	log.Fatal(err)
}
log.Print(o.Name)
```

`orderRepo` is a repository adapter. Its methods get the connection with `postgres.Conn(ctx, r.pool)`. The sample is `Example_command` in `example_command_test.go`.

## resources/test/containers

Purpose: start PostgreSQL and Valkey containers in tests.

Import path: `github.com/techorionai/constellation-platform-go/resources/test/containers`

Environment variables: None.

Use:

```go
var t testing.TB

pg := containers.Postgres(t)
dbURL, database := pg.NewDatabase(t) // a new database for this test
_, _ = dbURL, database
```

```go
var t testing.TB

v := containers.Valkey(t)
user, password := v.NewACLUser(t, "identity") // an ACL user with the key prefix rule
_, _, _ = v.Address(), user, password
```

In a real test, `t` is the argument of the test function.

Errors: a failed start calls `t.Fatalf`.

Rules and limits:

- Import this package from test code only. It imports `testing`. The `depguard` rule in `.golangci.yml` of this repository enforces it here. Add a similar rule in each service.
- Docker must run.
- Default images: `postgres:18-alpine` and `valkey/valkey:9-alpine`. Use `WithPostgresImage` and `WithValkeyImage` to change them.
- `NewDatabase` makes a database `test_<16 hex>` and drops it at cleanup. Tests in one package can share one container.
- `NewACLUser` uses the rule `containers.ACLRule`. It is the same rule as the sample in the `clients/valkey` section.
- `Stop(ctx)` stops a container before the cleanup.
- `AdminURL()`, `Exec`, `Admin(t)`, and `AdminPassword()` give the superuser access for the setup of a test.

## resources/test/issuer

Purpose: a test token issuer. A service uses it in handler tests, to get real signed tokens and a real JWKS endpoint.

Import path: `github.com/techorionai/constellation-platform-go/resources/test/issuer`

Environment variables: None.

Use:

```go
var t testing.TB

iss := issuer.New(t)
verifier, err := jwt.New(jwt.Config{Issuer: iss.Issuer(), Audience: "demo", JWKSURL: iss.URL()})
if err != nil {
	t.Fatal(err)
}
token := iss.Mint(t, issuer.Claims{Scope: "demo:ticket:read"})
_, err = verifier.Verify(context.Background(), token)
```

In a real test, `t` is the argument of the test function.

Errors: a failed key generation or signature calls `t.Fatalf`.

Rules and limits:

- `New` makes an ES256 key pair and starts an `httptest` server with `/.well-known/jwks.json`. The server stops at the end of the test.
- `Mint` signs a token. `Claims` has a default for each field. A zero field gives the default. `Extra` sets a claim to any value, and `Remove` removes a claim. `Alg`, `Typ`, and `Kid` change the header. `SigningKey` signs with a key that is not in the JWKS.
- `Alg` values `none`, `HS256`, and `RS256` make forged tokens for the tests of the verifier.
- `Rotate` adds a key and keeps the old keys. `RemoveKey` removes a key from the JWKS and keeps its private key, so a test can still sign with it.
- `SetStatus` and `SetBody` make the endpoint return an error status or a raw body. `Close` stops the server. `Fetches` counts the requests.
- `WithClock` sets the clock of the default `iat` and `exp`. Use it with `jwt.WithClock`.
- Import this package from test code only. The `depguard` rule `no-test-helpers-in-production` enforces it.

## resources/test/mock

Purpose: hold the mocks of ports. The package has mocks of `migrate.Locker`, `migrate.Migrator`, `health.Checker`, `cache.Store` (`CacheStore`), `transaction.Transactor` (`Transactor`), `secrets.Repository` (`SecretsRepository`), `retention.Purger` (`RetentionPurger`), `retention.Locker` (`RetentionLocker`), and `auth.Verifier` (`Verifier`).

Import path: `github.com/techorionai/constellation-platform-go/resources/test/mock`

Environment variables: None.

Use:

```go
l := &mock.Locker{}
m := &mock.Migrator{UpFunc: func(context.Context) (migrate.Result, error) {
	return migrate.Result{ToVersion: 2, Applied: 2}, nil
}}
res, err := migrate.Run(context.Background(), l, m, migrate.Options{})
fmt.Println(res.Applied, err, l.UnlockCalls())
// Output: 2 <nil> 1
```

Errors: None.

Rules and limits:

- A mock replaces a port only in a unit test. A server-side integration test uses a test container and a live dependency.
- Each mock has a function field for each method (`LockFunc`, `UnlockFunc`, `UpFunc`, `CheckFunc`, `VerifyFunc`). If the field is nil, the mock succeeds. Each mock counts its calls (`LockCalls`, `UnlockCalls`, `UpCalls`, `CheckCalls`, `VerifyCalls`).
- `CacheStore` keeps the values in a map and applies `cache.CheckKey`, `cache.CheckTTL`, and `cache.CheckValue`. It has the fields `GetFunc`, `SetFunc`, and `DeleteFunc`, the counts `GetCalls`, `SetCalls`, and `DeleteCalls`, and the helpers `Put`, `Value`, and `TTL`.
- `Transactor` calls `transaction.Resolve`, then calls `fn` without a database transaction. It follows the rules of the adapter for nested calls and `AfterCommit` hooks: the outermost call runs the hooks after `fn` returns nil, and a nested call joins the hooks of the outer call. `BeforeFunc` can return an error before `fn`. `WithinTxCalls` and `CommitCalls` count the calls.
- `SecretsRepository` keeps the records in a map and follows the rules of the PostgreSQL adapter. It has a function field for each method (`UpsertFunc`, `GetFunc`, `DeleteFunc`, `ListNamesFunc`, `ListForRewrapFunc`, `UpdateEnvelopeFunc`), call counters, the parameters of the list calls, and the helpers `Put` and `Record`.
- `RetentionPurger` has `PurgeFunc`, `PurgeCalls`, and `PurgeParams`. `RetentionLocker` acquires the lock by default. `Busy` makes it return `acquired` false, and `TryLockFunc` replaces the method. It counts `TryLockCalls`, `UnlockCalls`, and `LockNames`.
- A mock imports the package of its port. A unit test that imports `resources/test/mock` is in the external test package (`<package>_test`).
