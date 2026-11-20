package manifest_test

import (
	"context"
	"fmt"
	"log"
	"testing/fstest"

	"github.com/techorionai/constellation-platform-go/manifest"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

// ExampleLoad shows a manifest from a file system. A service embeds the file
// with //go:embed manifest.json.
func ExampleLoad() {
	files := fstest.MapFS{"manifest.json": {Data: []byte(`{
  "app": "helpdesk",
  "permissions": [{"key": "helpdesk:ticket:read", "description": "Read tickets."}],
  "defaultRoles": [{"key": "agent", "name": "Agent", "description": "An agent.", "permissions": ["helpdesk:ticket:read"]}]
}`)}}
	m, err := manifest.Load(files, "manifest.json")
	fmt.Println(m.App, len(m.Permissions), err)
	// Output: helpdesk 1 <nil>
}

func ExamplePublisher_Run() {
	reg := &mock.ManifestRegistry{} // a real service uses identity.NewManifestRegistry
	m := manifest.PermissionManifest{App: "helpdesk"}
	pub := manifest.NewPublisher(reg, m)
	fmt.Println("ready before:", pub.Checker().Check(context.Background()) == nil)
	if err := pub.Run(context.Background()); err != nil {
		log.Fatal(err)
	}
	fmt.Println("ready after:", pub.Checker().Check(context.Background()) == nil)
	// Output:
	// ready before: false
	// ready after: true
}
