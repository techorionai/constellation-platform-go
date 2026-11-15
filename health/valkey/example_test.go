package valkey_test

import (
	"context"
	"log"

	clientvk "github.com/techorionai/constellation-platform-go/clients/valkey"
	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/health"
	healthvk "github.com/techorionai/constellation-platform-go/health/valkey"
)

// ExampleNewChecker has no Output comment, because it needs a Valkey server.
func ExampleNewChecker() {
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
}
