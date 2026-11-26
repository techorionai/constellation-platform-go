package events_test

import (
	"context"
	"log"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/clients/events"
	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/eventtypes"
)

// ExampleNew shows the events client and the event type registry of a service.
// It has no Output comment, because it needs the events service.
func ExampleNew() {
	var cfg struct {
		Events events.Config `envPrefix:"EVENTS__"`
	}
	if err := config.Load("DEMO_", &cfg); err != nil {
		log.Fatal(err)
	}
	// In a service, src is the token source of auth/clientcred.
	var src auth.TokenSource
	c, err := events.New(cfg.Events, src, []string{"events:event_type:register"})
	if err != nil {
		log.Fatal(err)
	}
	var registry eventtypes.Registry = events.NewEventTypeRegistry(c)
	_, err = registry.Register(context.Background(), eventtypes.Registration{Source: "/demo", App: "demo"})
	log.Print(err)
}
