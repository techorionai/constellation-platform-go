package platform_test

import (
	"context"
	"log"
	"log/slog"

	"github.com/techorionai/constellation-platform-go/clients/postgres"
	"github.com/techorionai/constellation-platform-go/clients/valkey"
	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/telemetry"
)

// Example_startup shows the startup of a service. It has no Output comment,
// because it needs PostgreSQL and Valkey servers.
func Example_startup() {
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
	defer tel.Shutdown(ctx) //nolint:errcheck

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
}
