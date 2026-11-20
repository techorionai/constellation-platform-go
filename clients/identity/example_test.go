package identity_test

import (
	"context"
	"log"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/clients/identity"
	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/manifest"
)

// ExampleNew shows the identity client and the manifest registry of a service.
// It has no Output comment, because it needs the identity service.
func ExampleNew() {
	var cfg struct {
		Identity identity.Config `envPrefix:"IDENTITY__"`
	}
	if err := config.Load("DEMO_", &cfg); err != nil {
		log.Fatal(err)
	}
	// In a service, src is the token source of auth/clientcred.
	var src auth.TokenSource
	// The caller gives the scopes. The client asks for the audience "identity".
	c, err := identity.New(cfg.Identity, src, []string{"identity:manifest:publish"})
	if err != nil {
		log.Fatal(err)
	}
	var registry manifest.Registry = identity.NewManifestRegistry(c)
	err = registry.Publish(context.Background(), manifest.PermissionManifest{App: "demo"})
	log.Print(err)
}
