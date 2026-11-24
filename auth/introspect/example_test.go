package introspect_test

import (
	"context"
	"log"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/auth/introspect"
	"github.com/techorionai/constellation-platform-go/config"
)

// ExampleNew shows how a service accepts both JWT access tokens and personal
// API tokens. It has no Output comment, because it needs the identity service.
func ExampleNew() {
	var cfg struct {
		Introspect introspect.Config `envPrefix:"AUTH__INTROSPECT__"`
	}
	if err := config.Load("DEMO_", &cfg); err != nil {
		log.Fatal(err)
	}
	var client auth.ClientAssertion // a *clientcred.Source
	var jwtVerifier auth.Verifier   // a *jwt.Verifier
	apiVerifier, err := introspect.New(cfg.Introspect, client)
	if err != nil {
		log.Fatal(err)
	}
	verifier := auth.ByFormat(jwtVerifier, apiVerifier)
	p, err := verifier.Verify(context.Background(), "cpat_...")
	_, _ = p, err
}
