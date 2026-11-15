package telemetry_test

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/techorionai/constellation-platform-go/telemetry"
)

func ExampleSetup() {
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
}
