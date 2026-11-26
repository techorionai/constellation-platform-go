//go:build integration

package postgres_test

import (
	"testing"

	valkeygo "github.com/valkey-io/valkey-go"

	"github.com/techorionai/constellation-platform-go/resources/test/containers"
)

// aclClient returns a client for a new ACL user of the prefix with the event
// stream selectors.
func aclClient(t *testing.T, vk *containers.ValkeyInstance, prefix string) valkeygo.Client {
	t.Helper()
	user, password := vk.NewACLUser(t, prefix, containers.ValkeyACLEvents())
	c, err := valkeygo.NewClient(valkeygo.ClientOption{
		InitAddress: []string{vk.Address()}, Username: user, Password: password.Reveal(), DisableCache: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}
