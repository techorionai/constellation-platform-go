# Conventions

These rules apply to this library and to each service that uses it.

## Package layout

`platform-go` is a library. Other repositories import its packages, so the code is not in `internal/`.

- The root package of a concern holds the port interfaces and the logic that has no infrastructure imports.
- An adapter is in a subpackage, for example `health/postgres`. An adapter imports its port package. It does not import other adapters.
- Each adapter has a compile-time interface check.
- `clients/` holds the low-level constructors for infrastructure clients.
- `resources/test/containers/`, `resources/test/issuer/`, and `resources/test/mock/` hold the test helpers. They are normal packages, so that service repositories can import them.
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

A record that the code writes with a request context (`slog.InfoContext(ctx, ...)`) behind `requestid.Middleware` and `auth.Middleware` also has `request.id`, `principal.id`, `principal.type`, and `client.id`. `telemetry.ContextWithAttrs` adds such fields. The `telemetry` package does not import `auth` or `requestid`.

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

### Metrics of the executors, the cache, and the transactions

Metric attributes never contain keys, values, or other unbounded data. The names of namespaces, executors, and transactions are fixed in the code, so the cardinality stays low.

| Metric | Type | Unit | Attributes | Meaning |
| ------ | ---- | ---- | ---------- | ------- |
| `executor.duration` | Histogram | `s` | `executor.name`, `outcome` (`ok`, `error`, `expected_error`) | Duration of one executor call. |
| `cache.requests` | Counter | `{request}` | `cache.namespace`, `cache.operation` (`get`, `set`, `delete`), `result` (`hit`, `miss`, `ok`, `error`) | One count for each store call. |
| `cache.duration` | Histogram | `s` | `cache.namespace`, `cache.operation`, `result` | Time of each store call. |
| `cache.value.size` | Histogram | `By` | `cache.namespace`, `cache.operation` (`get`, `set`) | Size of the encoded value on a hit and on a set. |
| `cache.load.duration` | Histogram | `s` | `cache.namespace`, `outcome` (`ok`, `error`) | Time of the load function of `GetOrLoad`. |
| `cache.load.shared` | Counter | `{call}` | `cache.namespace` | `GetOrLoad` calls that used the load result of another call. |
| `cache.decode.errors` | Counter | `{error}` | `cache.namespace` | Stored values that do not decode. |
| `constellation.db.transaction.duration` | Histogram | `s` | `db.transaction.name`, `db.transaction.isolation`, `db.transaction.read_only`, `outcome` | Time from the start of the transaction to the end of the commit or the rollback. The count gives the rate of transactions. |
| `constellation.db.transaction.active` | UpDownCounter | `{transaction}` | `db.transaction.name` | Open transactions now. |
| `constellation.db.transaction.begin.duration` | Histogram | `s` | `db.transaction.name` | Time to start a transaction, with the wait for a connection. |
| `constellation.db.transaction.serialization_failures` | Counter | `{failure}` | `db.transaction.name`, `sqlstate` (`40001`, `40P01`) | Serialization failures and deadlocks. |
| `constellation.db.transaction.nested` | Counter | `{call}` | `db.transaction.name` | `WithinTx` calls that joined an outer transaction. |

- The hit ratio of the cache is `cache.requests{result="hit"}` divided by all `get` requests.
- Values of `outcome` of `constellation.db.transaction.duration`: `commit`, `rollback` (`fn` returned an error), `panic`, `goexit` (`fn` called `runtime.Goexit`), `commit_error` (`fn` returned nil, but the commit failed), and `begin_error` (the transaction did not start).
- The transaction metrics start with `constellation.`, because the OpenTelemetry semantic conventions reserve the `db.` namespace and do not define these metrics. The span name `db.transaction` and the attribute names do not change.
- Histogram boundaries. The default boundaries of the SDK (0 to 10000) suit milliseconds, not seconds or bytes, so each histogram sets its own:

  | Unit | Boundaries |
  | ---- | ---------- |
  | `s` | 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10 |
  | `By` | 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576 |

### Retention metrics

| Metric | Type | Unit | Attributes | Meaning |
| ------ | ---- | ---- | ---------- | ------- |
| `retention.deleted` | Counter | `{row}` | `job` | The rows that a tick deleted. |
| `retention.duration` | Histogram | `s` | `job`, `outcome` | The time of one tick. |
| `retention.last_success` | Gauge | `s` | `job` | The Unix time of the end of the last tick with the outcome `ok` in this instance. |

Values of `outcome` of `retention.duration`:

| Value | Meaning |
| ----- | ------- |
| `ok` | The instance had the lock, and all `Purge` calls worked. |
| `error` | `TryLock` or a `Purge` call returned an error that is not a context error of `ctx`. |
| `canceled` | `ctx` ended during the tick, for example at a normal stop of the service. |
| `skipped` | Another instance had the lock. |

- A normal stop gives `canceled`, so an alert on `outcome=error` does not fire at a normal stop. A tick that stops because `ctx` ended is logged at `info` level. Another error is logged at `error` level.
- A skipped tick records `retention.duration` with `outcome=skipped`. It does not record `retention.deleted` or `retention.last_success`.
- Only the instance that has the lock updates `retention.last_success`. An alert reads the maximum of `retention.last_success` over all instances for each `job`. Example rule: the maximum is older than 3 times the interval of the job.
- Also alert on a rate of `retention.duration{outcome="error"}` that is more than 0 for a period longer than one interval.

### Authentication metrics

| Metric | Type | Unit | Attributes | Meaning |
| ------ | ---- | ---- | ---------- | ------- |
| `constellation.auth.verify` | Counter | `{verification}` | `outcome` (`ok`, `no_token`, `invalid`, `expired`, `wrong_issuer`, `wrong_audience`, `keys_unavailable`, `unavailable`) | One count for each request that `auth.Middleware` verifies. `RequireScopes` and `RequireMFA` are not counted. |
| `constellation.auth.jwks.fetch` | Counter | `{fetch}` | `outcome` (`ok`, `http_error`, `bad_body`, `too_large`, `no_keys`, `network_error`) | One count for each JWKS fetch of `auth/jwt`. |
| `constellation.auth.token.request` | Counter | `{request}` | `outcome` (`ok`, `oauth_error`, `unavailable`, `bad_response`, `network_error`) | One count for each token request of `auth/clientcred`. A cache hit makes no request and is not counted. |
| `constellation.auth.token.exchange` | Counter | `{request}` | `outcome` (`ok`, `oauth_error`, `unavailable`, `bad_response`, `network_error`) | One count for each token exchange request of `auth/clientcred`. A cache hit makes no request and is not counted. |
| `constellation.auth.introspect` | Counter | `{verification}` | `outcome` (`active`, `inactive`, `cached_active`, `cached_inactive`, `unavailable`, `client_rejected`, `invalid_response`, `invalid_format`, `overloaded`) | One count for each API token verification of `auth/introspect`. |
| `constellation.auth.introspect.duration` | Histogram | `s` | none | Duration of the HTTP call to the introspection endpoint. |

Spans: `auth.clientcred.token` (token request), `auth.clientcred.exchange` (token exchange, attributes `constellation.auth.client_id`, `constellation.auth.audience`, `constellation.auth.exchange.with_agent`, and `constellation.auth.outcome`), and `auth.introspect.request` (introspection call). An authenticated request span also has `constellation.principal.token_type`, and with an actor `constellation.principal.act.id` and `constellation.principal.act.type`.

### Manifest metrics

| Metric | Type | Unit | Attributes | Meaning |
| ------ | ---- | ---- | ---------- | ------- |
| `constellation.manifest.publish` | Counter | `{attempt}` | `outcome` (`ok`, `invalid`, `rejected`, `denied`, `unavailable`) | One count for each publish attempt of the `manifest` publisher. An alert on `denied` shows a wrong client config. An alert on `invalid` shows a wrong manifest. An alert on `rejected` shows a fixed 4xx refusal of the registry. |

An alert on a high rate of `keys_unavailable` or of `network_error` shows that the service cannot reach the identity service. An alert on a high rate of `wrong_audience` can show a client that calls the wrong service.

## Authentication

- Never log a token. Never put a token in a span attribute, a metric attribute, or an error body.
- Never read the principal from a header or from baggage. The principal comes only from a token that `auth.Middleware` verified.
- Each service checks the audience and the scopes of each request. Use `auth.Middleware` with the `AppKey` of the service as the audience, and `auth.RequireScopes` on each route.
- The order of the middleware is `requestid.Middleware`, then `auth.Middleware`, then `auth.RequireScopes` for each route.
- A sensitive action uses `auth.RequireMFA`. If the handler has checks that come first, it calls `auth.CheckMFA` in the handler. Check `mfa` only. Do not check other `amr` values.
- Only `auth/jwt`, `auth/clientcred`, and the test issuer import the JOSE library. The adapter `auth/introspect` gets its client assertion from the port `auth.ClientAssertion`, and a `depguard` rule denies it the JOSE library and `clients/identity`. A `depguard` rule keeps it out of `auth` and `requestid`.
- Never log a private key, an assertion, or a token. Never put one in a span attribute, a metric attribute, or an error text.
- The introspection cache of `auth/introspect` is in memory and not in Valkey. Each service has its own Valkey key prefix and ACL user, and cannot read the keys of the identity service. A revoked API token works for a maximum of `CacheTTL` (60 s at most) in each service instance.
- A cache key that comes from a token is a SHA-256 hash. The cache never holds the raw token as a key. This applies to `auth/introspect` and to the exchange cache of `auth/clientcred`.
- Use `auth.ByFormat(jwtVerifier, introspectVerifier)` to accept both JWT access tokens and personal API tokens. A service that must refuse API tokens on a route checks `Principal.TokenType`.
- A client of a platform service treats the status 429 as "unavailable" and waits for the `Retry-After` header before the next call. It does the same for the status 503 with a valid `Retry-After` header. A 503 with no valid header starts no wait time. `auth.RetryAfter` and `manifest.RetryAfter` return the wait time of an error. A call that the wait time stops makes no request and is not counted in the metric, the same as a cache hit.
- The outcome `overloaded` of `constellation.auth.introspect` also covers the local limit `MaxCallsPerSecond` of `auth/introspect`.

## Generated code

- The generated identity client is `clients/identity/identity.gen.go`. It comes from `api/openapi/Constellation.Identity.openapi.yaml`, a copy of a file of a `constellation-contracts` release tag. The tag is in `api/openapi/CONTRACTS_VERSION`.
- Never change a generated file or the OpenAPI copy by hand. A change goes into `constellation-contracts` first.
- CI runs `go generate ./... && git diff --exit-code` before the unit tests. A stale generated file fails the CI.
- The linters skip files with the `Code generated` header. Only `clients/identity` can import the `oapi-codegen` runtime. A `depguard` rule enforces it.

## Executors and decorators

- A command or a query implements `executor.Executor[P]` or `executor.ExecutorWithReturn[P, R]`. A decorator implements the same interface.
- Order of the decorators, innermost first: the command, then logging, then tracing. Tracing is the outermost decorator, so the log records have the `trace_id` of the executor span. The sample in section 9 of the hexagonal architecture guide puts logging outermost. This library puts tracing outermost, because only then the log records carry the trace ID.
- A decorator does not log the params or the result, because they can contain secrets and personal data.
- The error text goes to the log record and to the span. Some error texts contain input values, for example the PostgreSQL error `invalid input syntax for type uuid: "<value>"`. The text of an error that a command returns must not contain secrets or personal data.
- Use `decorators.WithErrorClassifier` to mark expected errors (validation errors, "not found"). The span keeps the status `Unset` and `outcome` is `expected_error`. Thus expected errors do not increase the error rate. Use `decorators.WithErrorLevel` to log an expected error at the `info` level.

## Cache

- The cache is not the source of truth. A cache failure must not stop a request. `Cache.GetOrLoad` calls the load function when the cache fails and writes a `warn` log record.
- A cache entry always has a TTL, from 1 millisecond to 30 days. Use a short TTL for data that changes.
- A span or a log record never contains a key or a value, because keys can contain personal data. No metric attribute contains a key or a value.
- Do not put secrets or decrypted secret values in the cache. Valkey data has no encryption at rest. The default JSON codec writes a `config.Secret` field as `"[redacted]"` without an error, so a value with such a field loses the secret in the cache.
- In `WithinTx`, do not call `Set` or `Delete` directly. Register the call with `Transactor.AfterCommit`. Then another request cannot store the old value again before the commit.
- Do not call `GetOrLoad` in a `WithinTx` that changes the data that `load` reads. Else the cache can store data that the transaction then rolls back.

## Transactions

- Use `clients/postgres.Conn(ctx, pool)` in each repository adapter, for each call. Then the repository joins the transaction of the caller. A repository that uses the pool directly does not join the transaction.
- Only the transaction adapter (`transaction/postgres`) calls `clients/postgres.ContextWithTx`. A context with a transaction that the adapter did not start makes `AfterCommit` panic and `WithinTx` return `ErrForeignTx`.
- An `AfterCommit` hook must obey its context. The adapter gives each hook a time limit (`WithHookTimeout`, default 5 seconds). A hook that does not end in the time limit gets an `error` log record, and its goroutine ends later.
- A call of `AfterCommit` after the outermost transaction ended, for example from a goroutine that `fn` started, drops the hook and writes an `error` log record.
- A nested `WithinTx` joins the outer transaction and can set `Name` only. There are no savepoints.

## Secrets

- A service stores a secret with `secrets.Service`. The value is encrypted with a random data key (AES-256-GCM). The master key wraps the data key. The database holds only ciphertext.
- Name rule: `^[a-z0-9][a-z0-9._:/-]{0,199}$`. A value has 1 byte to 64 KiB.
- AAD labels: the data uses `constellation-secrets:v1:data:<name>`. The wrap of the data key uses `constellation-secrets:v1:kek:<key id>`. Thus a ciphertext that is copied to a row with a different name does not decrypt, and the two uses never share an AAD.
- A secret value or a master key is never in a log record, a span, a metric attribute, or an error. A span has the attribute `secret.name`. A log record about a secret has the name and the kind of the error only.
- The master key comes from `<PREFIX>SECRETS__MASTER_KEY` or `<PREFIX>SECRETS__MASTER_KEY_FILE`. Key IDs match `^[a-z0-9-]{1,32}$`. Never write a real key in a test, in a sample, or in the documentation.
- A backup holds the encrypted secrets and not the master key. The operator keeps the master key in a different place. A lost master key makes all secrets lost.
- The table `platform_secrets` has its own migrations table, `platform_secrets_schema_migrations`. The service runs `migrate.Run` a second time for it, with the lock name, the migrations table, and `Options.Name` all equal to `secretspg.MigrationsTable`.
- A later change to `platform_secrets` follows the expand and contract rule. Each migration works with the current and the previous release of `platform-go`.
- The calling service checks the permissions and writes the audit entries for the access to a secret. `secrets.Service` does neither.

## Retention

- Each retention job has a name that matches `^[a-z][a-z0-9_]{0,63}$`. The name is the value of the metric attribute `job`, and the lock name is `retention:<name>`.
- A service puts one `retention.Config` for each job in its config struct, with `envPrefix:"RETENTION__<JOB>__"` and the job name in upper case. Variables: `PERIOD`, `INTERVAL` (default `1h`, 1 minute or more), and `BATCH_SIZE` (default `1000`, up to `100000`). Example: `IDENTITY_RETENTION__AUDIT__PERIOD=1440h`.
- The service sets `Job.DefaultPeriod` in code, from `VISION.md` section 15. `Config.Period` wins when it is more than 0.
- Only one instance runs a job at a time. A tick that finds the lock with another instance is skipped.
- A table purger works with these rules: the time column has the type `timestamptz`. The key column is the primary key, or a part of the primary key of a partitioned table. Add an index on the time column, because the delete reads the oldest rows first.
- The retention adapters use the pool directly. A tick is not part of a caller transaction.
- A tick uses 2 pool connections: the lock connection for the full tick, and one connection for each `Purge` call. Set `MaxConns` with spare connections for the jobs, in addition to the connections for requests.
- A job does not delete from Valkey Streams. The outbox and relay issue (roadmap R08) trims them.

## Tests

- Integration tests have the build tag `integration`: `//go:build integration`. Run them with `go test -race -tags=integration ./...`.
- Server-side integration tests use the test containers of `resources/test/containers` and live dependencies. They do not use mocks or stubs.
- A mock goes in `resources/test/mock` and replaces a port only in a unit test.
- A unit test that imports `resources/test/mock` is in the external test package (`<package>_test`), because the mocks import the package under test.
- Put the test ID of the plan in the test name or in a comment above the test.
