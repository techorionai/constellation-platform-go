# constellation-platform-go

Shared Go library of the Constellation platform. Each service repository imports its packages.

Packages in this release: `config`, `telemetry`, `clients/postgres`, `clients/valkey`, `migrate`, `migrate/postgres`, `health`, `health/postgres`, `health/valkey`, `executor`, `decorators`, `cache`, `cache/valkey`, `transaction`, `transaction/postgres`, `secrets`, `secrets/postgres`, `retention`, `retention/postgres`, `requestid`, `auth`, `auth/jwt`, `auth/introspect`, `auth/clientcred`, `clients/identity`, `manifest`, `resources/test/containers`, `resources/test/issuer`, and `resources/test/mock`.

## Layout

```txt
config/                      Env var loader
telemetry/                   OpenTelemetry setup
clients/postgres/            pgxpool constructor
clients/valkey/              valkey-go constructor
clients/identity/            Generated identity client and manifest registry adapter
api/openapi/                 OpenAPI file and tag of the contracts release
manifest/                    Permission manifest publisher, port, and loader
requestid/                   Request ID middleware
auth/                        Principal, verifier port, scope checks, HTTP middleware
auth/jwt/                    JWKS verifier for access tokens
auth/introspect/             API token verifier with introspection
auth/clientcred/             Client credentials token source and token exchange
resources/test/containers/   PostgreSQL and Valkey containers for tests
resources/test/issuer/       Test token issuer with a JWKS endpoint
resources/test/mock/         Mocks of ports
docs/                        Conventions and usage
```

## Commands

| Command | Use |
| ------- | --- |
| `go generate ./...` | Generate the identity client again. Run it after you change `api/openapi`. |
| `go test -race ./...` | Run the unit tests. |
| `go test -race -tags=integration ./...` | Run the unit and integration tests. Docker must run. |
| `golangci-lint run` | Run the linters. |

## Use

```sh
go get github.com/techorionai/constellation-platform-go@vX.Y.Z
```

Read [docs/usage.md](docs/usage.md) for the packages and [docs/conventions.md](docs/conventions.md) for the rules.

## License

MIT. See [LICENSE](LICENSE).
