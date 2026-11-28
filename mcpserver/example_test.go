package mcpserver_test

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/mcpserver"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

// ExampleAddTool shows a read tool with one scope, and the mount of the handlers.
func ExampleAddTool() {
	verifier := &mock.Verifier{} // a real service uses auth.ByFormat(jwt, introspect)
	s, err := mcpserver.New(mcpserver.Config{
		App: "demo", Name: "demo", Version: "1.0.0", Verifier: verifier,
		ResourceURL:          "https://demo.example.com/mcp",
		AuthorizationServers: []string{"https://identity.example.com"},
	})
	if err != nil {
		log.Fatal(err)
	}
	type in struct {
		ID string `json:"id" jsonschema:"item ID"`
	}
	type out struct {
		ID string `json:"id"`
	}
	err = mcpserver.AddTool(s, mcpserver.ToolSpec{
		Name: "get_item", Description: "Get one item.",
		Scopes: []string{"demo:item:read"}, ReadOnly: true,
	}, func(_ context.Context, _ auth.Principal, i in) (out, error) {
		return out{ID: i.ID + "!"}, nil
	})
	fmt.Println(err)

	mux := http.NewServeMux()
	mux.Handle("/mcp", s.Handler())
	mux.Handle("/.well-known/oauth-protected-resource/mcp", s.MetadataHandler())
	// Output: <nil>
}
