package containers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcvalkey "github.com/testcontainers/testcontainers-go/modules/valkey"
	valkeygo "github.com/valkey-io/valkey-go"

	"github.com/techorionai/constellation-platform-go/config"
)

// DefaultValkeyImage is the default Valkey image.
const DefaultValkeyImage = "valkey/valkey:9-alpine"

// ACLRule is the command rule of the ACL users that NewACLUser makes. It is the
// same rule as the sample in docs/usage.md.
const ACLRule = "+@all -@admin -@dangerous -function -script|flush -script|kill"

const valkeyAdminPassword = "admin-password"

type valkeyOptions struct {
	image string
}

// ValkeyOption changes the Valkey container.
type ValkeyOption func(*valkeyOptions)

// WithValkeyImage sets the image of the Valkey container.
func WithValkeyImage(image string) ValkeyOption {
	return func(o *valkeyOptions) { o.image = image }
}

// ValkeyInstance is a running Valkey container. The default user has a
// password.
type ValkeyInstance struct {
	ctr     *tcvalkey.ValkeyContainer
	address string

	mu      sync.Mutex
	stopped bool
}

// Valkey starts a Valkey container, waits for it, and registers t.Cleanup to
// stop it.
func Valkey(t testing.TB, opts ...ValkeyOption) *ValkeyInstance {
	t.Helper()
	o := valkeyOptions{image: DefaultValkeyImage}
	for _, opt := range opts {
		opt(&o)
	}
	ctx := context.Background()
	ctr, err := tcvalkey.Run(ctx, o.image, testcontainers.WithCmdArgs("--requirepass", valkeyAdminPassword))
	if ctr != nil {
		t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })
	}
	if err != nil {
		t.Fatalf("containers: start valkey: %v", err)
	}
	raw, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("containers: valkey connection string: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("containers: parse valkey URL: %v", err)
	}
	return &ValkeyInstance{ctr: ctr, address: u.Host}
}

// Address returns the host and port of the server.
func (v *ValkeyInstance) Address() string { return v.address }

// AdminPassword returns the password of the default user.
func (v *ValkeyInstance) AdminPassword() config.Secret { return valkeyAdminPassword }

// Admin returns a client that uses the default user. It closes the client at
// cleanup.
func (v *ValkeyInstance) Admin(t testing.TB) valkeygo.Client {
	t.Helper()
	c, err := valkeygo.NewClient(valkeygo.ClientOption{
		InitAddress:  []string{v.address},
		Password:     valkeyAdminPassword,
		DisableCache: true,
	})
	if err != nil {
		t.Fatalf("containers: admin client: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// NewACLUser makes an ACL user that can access the keys ~<prefix>:* only, with
// the command rule [ACLRule].
func (v *ValkeyInstance) NewACLUser(t testing.TB, prefix string) (string, config.Secret) {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("containers: random name: %v", err)
	}
	user := prefix + "_" + hex.EncodeToString(b)
	password := hex.EncodeToString(b) + "pw"
	admin := v.Admin(t)
	args := []string{"ACL", "SETUSER", user, "on", ">" + password, "~" + prefix + ":*"}
	args = append(args, strings.Fields(ACLRule)...)
	if err := admin.Do(context.Background(), admin.B().Arbitrary(args[0]).Args(args[1:]...).Build()).Error(); err != nil {
		t.Fatalf("containers: ACL SETUSER: %v", err)
	}
	return user, config.Secret(password)
}

// Stop stops the container before the cleanup.
func (v *ValkeyInstance) Stop(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stopped {
		return nil
	}
	v.stopped = true
	return v.ctr.Stop(ctx, nil)
}
