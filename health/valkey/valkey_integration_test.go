//go:build integration

package valkey_test

import (
	"context"
	"testing"
	"time"

	valkeygo "github.com/valkey-io/valkey-go"

	healthvk "github.com/techorionai/constellation-platform-go/health/valkey"
	"github.com/techorionai/constellation-platform-go/resources/test/containers"
)

// I9: the container is not shared with other tests.
func TestIntegrationCheckStoppedServer(t *testing.T) {
	v := containers.Valkey(t)
	client, err := valkeygo.NewClient(valkeygo.ClientOption{
		InitAddress:  []string{v.Address()},
		Password:     v.AdminPassword().Reveal(),
		DisableCache: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	checker := healthvk.NewChecker(client)
	if checker.Name() != "valkey" {
		t.Fatalf("name = %q", checker.Name())
	}
	if err := checker.Check(t.Context()); err != nil {
		t.Fatalf("first check: %v", err)
	}
	if err := v.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := checker.Check(ctx); err == nil {
		t.Fatal("check after stop: want error")
	}
}
