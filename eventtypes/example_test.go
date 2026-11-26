package eventtypes_test

import (
	"context"
	"fmt"
	"log"
	"testing/fstest"

	"github.com/techorionai/constellation-platform-go/eventtypes"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

const exampleFile = `{
  "source": "/helpdesk",
  "app": "helpdesk",
  "eventTypes": [{
    "type": "helpdesk.ticket.created",
    "description": "A ticket was created.",
    "dataSchema": {"type": "object", "properties": {"id": {"type": "string"}}}
  }]
}`

// ExampleLoad shows a registration from a file system. A service embeds the file
// with //go:embed event-types.json and calls Load in a unit test.
func ExampleLoad() {
	files := fstest.MapFS{"event-types.json": {Data: []byte(exampleFile)}}
	reg, err := eventtypes.Load(files, "event-types.json")
	fmt.Println(reg.App, reg.Types(), err)
	// Output: helpdesk [helpdesk.ticket.created] <nil>
}

func ExamplePublisher_Run() {
	files := fstest.MapFS{"event-types.json": {Data: []byte(exampleFile)}}
	reg, err := eventtypes.Load(files, "event-types.json")
	if err != nil {
		log.Fatal(err)
	}
	registry := &mock.EventTypeRegistry{} // a real service uses events.NewEventTypeRegistry
	pub := eventtypes.NewPublisher(registry, reg)
	fmt.Println("ready before:", pub.Checker().Check(context.Background()) == nil)
	if err := pub.Run(context.Background()); err != nil {
		log.Fatal(err)
	}
	fmt.Println("ready after:", pub.Checker().Check(context.Background()) == nil)
	// Output:
	// ready before: false
	// ready after: true
}
