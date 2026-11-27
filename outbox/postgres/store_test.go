package postgres_test

import (
	"errors"
	"testing"

	"github.com/techorionai/constellation-platform-go/outbox"
	outboxpg "github.com/techorionai/constellation-platform-go/outbox/postgres"
)

// U9: the table name of NewStoreFor.
func TestU9NewStoreFor(t *testing.T) {
	if _, err := outboxpg.NewStoreFor(nil, "platform_audit_outbox"); err != nil {
		t.Errorf("valid name: %v", err)
	}
	for _, name := range []string{"bad name", "x;drop", "", "Upper", "1abc"} {
		if _, err := outboxpg.NewStoreFor(nil, name); !errors.Is(err, outbox.ErrInvalidConfig) {
			t.Errorf("NewStoreFor(%q) err = %v, want ErrInvalidConfig", name, err)
		}
	}
}
