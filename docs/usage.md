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
| [`resources/test/containers`](#resourcestestcontainers) | Start PostgreSQL and Valkey containers in tests. |
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

## resources/test/mock

Purpose: hold the mocks of ports. The package has only a package comment at this time.

Import path: `github.com/techorionai/constellation-platform-go/resources/test/mock`

Environment variables: None.

Use: None.

Errors: None.

Rules and limits: a mock replaces a port only in a unit test. A server-side integration test uses a test container and a live dependency.
