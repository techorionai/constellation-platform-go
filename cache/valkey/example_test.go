package valkey_test

import (
	"context"
	"log"
	"time"

	"github.com/techorionai/constellation-platform-go/cache"
	cachevk "github.com/techorionai/constellation-platform-go/cache/valkey"
	clientvk "github.com/techorionai/constellation-platform-go/clients/valkey"
	"github.com/techorionai/constellation-platform-go/config"
)

type Profile struct {
	Name string `json:"name"`
}

// ExampleNewStore has no Output comment, because it needs a Valkey server.
func ExampleNewStore() {
	ctx := context.Background()

	var cfg struct {
		Valkey clientvk.Config `envPrefix:"VALKEY__"`
	}
	if err := config.Load("IDENTITY_", &cfg); err != nil {
		log.Fatal(err)
	}
	kv, err := clientvk.New(ctx, cfg.Valkey)
	if err != nil {
		log.Fatal(err)
	}
	defer kv.Close()

	profiles := cache.New[Profile](cachevk.NewStore(kv), "profile")
	p, err := profiles.GetOrLoad(ctx, "user:42", time.Minute,
		func(context.Context) (Profile, error) { return Profile{Name: "Ada"}, nil })
	if err != nil {
		log.Fatal(err)
	}
	log.Print(p.Name)
}
