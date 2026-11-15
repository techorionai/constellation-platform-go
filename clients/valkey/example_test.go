package valkey_test

import (
	"context"
	"fmt"
	"log"

	"github.com/techorionai/constellation-platform-go/clients/valkey"
	"github.com/techorionai/constellation-platform-go/config"
)

func ExampleKeyspace() {
	ks, err := valkey.NewKeyspace("identity")
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(ks.Key("session", "42"))
	fmt.Println(ks.Pattern("session"))
	// Output:
	// identity:session:42
	// identity:session:*
}

// ExampleNew shows the startup of a service. It has no Output comment, because
// it needs a Valkey server.
func ExampleNew() {
	ctx := context.Background()

	var cfg struct {
		Valkey valkey.Config `envPrefix:"VALKEY__"`
	}
	if err := config.Load("IDENTITY_", &cfg); err != nil {
		log.Fatal(err)
	}
	client, err := valkey.New(ctx, cfg.Valkey)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	// Make each key with the Keyspace, so that it has the key prefix.
	key := client.Keys.Key("session", "42")
	cmd := client.Raw.B().Set().Key(key).Value("1").Build()
	if err := client.Raw.Do(ctx, cmd).Error(); err != nil {
		log.Fatal(err)
	}
}
