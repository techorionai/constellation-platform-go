# constellation-platform-go

Shared Go library of the Constellation platform. Each service repository imports its packages.

Packages in this release: `config`, `telemetry`, `clients/postgres`, `clients/valkey`, `resources/test/containers`, and `resources/test/mock`.

## Layout

```txt
config/                      Env var loader
telemetry/                   OpenTelemetry setup
clients/postgres/            pgxpool constructor
clients/valkey/              valkey-go constructor
resources/test/containers/   PostgreSQL and Valkey containers for tests
resources/test/mock/         Mocks of ports
docs/                        Conventions and usage
```

## Commands

| Command | Use |
| ------- | --- |
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
