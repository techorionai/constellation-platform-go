package platform_test

import (
	"context"
	"log"
	"log/slog"
	"testing/fstest"

	"github.com/techorionai/constellation-platform-go/clients/postgres"
	"github.com/techorionai/constellation-platform-go/clients/valkey"
	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/health"
	healthpg "github.com/techorionai/constellation-platform-go/health/postgres"
	healthvk "github.com/techorionai/constellation-platform-go/health/valkey"
	"github.com/techorionai/constellation-platform-go/migrate"
	migratepg "github.com/techorionai/constellation-platform-go/migrate/postgres"
	"github.com/techorionai/constellation-platform-go/telemetry"
)

// Example_startupOrder shows the startup order of a service with migrations and
// the admin server. It has no Output comment, because it needs PostgreSQL and
// Valkey servers.
func Example_startupOrder() {
	ctx := context.Background()

	var cfg struct {
		LogLevel slog.Level      `env:"LOG_LEVEL" envDefault:"INFO"`
		Database postgres.Config `envPrefix:"DATABASE__"`
		Valkey   valkey.Config   `envPrefix:"VALKEY__"`
		Admin    health.Config   `envPrefix:"ADMIN__"`
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

	// 6. Start the background jobs and the API server.
	// 7. Tell the state that the service is ready.
	state.MarkStarted()

	// 8. On a stop signal, do these steps in this order:
	_ = admin.Drain(ctx) // the load balancer sees 503 during DrainDelay
	// Stop the API server and the jobs.
	pgCheck.Close()
	kv.Close()
	db.Close()
	_ = tel.Shutdown(ctx)
	_ = admin.Shutdown(ctx) // the admin server stops last
}
