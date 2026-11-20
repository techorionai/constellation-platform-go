package clientcred_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/auth/clientcred"
	"github.com/techorionai/constellation-platform-go/config"
)

// ExampleNew shows the token source of a service. It has no Output comment,
// because it needs the identity service.
func ExampleNew() {
	var cfg struct {
		Client clientcred.Config `envPrefix:"CLIENT__"`
	}
	// DEMO_CLIENT__ID, DEMO_CLIENT__KEY_ID, DEMO_CLIENT__ISSUER, and
	// DEMO_CLIENT__PRIVATE_KEY (or DEMO_CLIENT__PRIVATE_KEY_FILE) are required.
	if err := config.Load("DEMO_", &cfg); err != nil {
		log.Fatal(err)
	}
	src, err := clientcred.New(cfg.Client)
	if err != nil {
		log.Fatal(err)
	}
	token, err := src.Token(context.Background(), "identity", "identity:manifest:publish")
	if err != nil {
		log.Fatal(err)
	}
	_ = token // never log the token
}

// ExampleSource_Token shows the errors of Token.
func ExampleSource_Token() {
	var src auth.TokenSource // a *clientcred.Source
	_, err := src.Token(context.Background(), "identity", "identity:manifest:publish")
	var oe *auth.OAuthError
	switch {
	case err == nil:
	case errors.As(err, &oe):
		fmt.Println("the identity service refused the client:", oe.Code)
	case errors.Is(err, auth.ErrTokenUnavailable):
		fmt.Println("try again later:", err)
	default:
		fmt.Println("bad answer:", err)
	}
}

// ExampleWithHTTPClient shows a custom HTTP client, for example in a test.
func ExampleWithHTTPClient() {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	_, _ = clientcred.New(clientcred.Config{}, clientcred.WithHTTPClient(srv.Client()))
}
