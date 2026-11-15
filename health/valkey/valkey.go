package valkey

import (
	"context"

	valkeygo "github.com/valkey-io/valkey-go"

	"github.com/techorionai/constellation-platform-go/health"
)

var _ health.Checker = (*Checker)(nil)

// Checker checks Valkey with PING.
type Checker struct {
	client valkeygo.Client
}

// NewChecker makes a checker for the valkey-go client, for example Client.Raw of
// clients/valkey.
func NewChecker(client valkeygo.Client) *Checker { return &Checker{client: client} }

// Name returns "valkey".
func (c *Checker) Name() string { return "valkey" }

// Check sends PING.
func (c *Checker) Check(ctx context.Context) error {
	return c.client.Do(ctx, c.client.B().Ping().Build()).Error()
}
