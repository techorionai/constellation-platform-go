package streams_test

import (
	"fmt"

	"github.com/techorionai/constellation-platform-go/streams"
)

func ExampleStreamKey() {
	fmt.Println(streams.StreamKey("identity"))
	// Output: events.stream:identity
}

func ExampleGroupName() {
	g, _ := streams.GroupName("helpdesk", "sla")
	fmt.Println(g)
	fmt.Println(streams.ValidGroup("helpdesk", "identity") != nil)
	// Output:
	// helpdesk.sla
	// true
}
