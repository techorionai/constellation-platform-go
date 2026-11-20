package jwt_test

import (
	"context"
	"log"
	"net/http"

	"github.com/techorionai/constellation-platform-go/auth"
	authjwt "github.com/techorionai/constellation-platform-go/auth/jwt"
	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/requestid"
)

// ExampleNew shows the verifier of a service. It has no Output comment, because
// it needs the identity service. A test uses resources/test/issuer in place of
// the identity service.
func ExampleNew() {
	ctx := context.Background()
	var cfg struct {
		Auth authjwt.Config `envPrefix:"AUTH__"`
	}
	// DEMO_AUTH__ISSUER and DEMO_AUTH__AUDIENCE are required.
	if err := config.Load("DEMO_", &cfg); err != nil {
		log.Fatal(err)
	}
	verifier, err := authjwt.New(cfg.Auth)
	if err != nil {
		log.Fatal(err)
	}
	verifier.Start(ctx) // optional background refresh
	defer verifier.Close()

	mux := http.NewServeMux()
	mux.Handle("GET /tickets", auth.RequireScopes("demo:ticket:read")(http.NotFoundHandler()))
	handler := requestid.Middleware()(auth.Middleware(verifier)(mux))
	log.Fatal(http.ListenAndServe(":8080", handler)) //nolint:gosec // sample only
}
