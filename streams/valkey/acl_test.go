package valkey_test

import (
	"testing"

	streamsvalkey "github.com/techorionai/constellation-platform-go/streams/valkey"
)

// U22: the text of the ACL selectors.
func TestU22ACLSelectors(t *testing.T) {
	want := "(~events.stream:helpdesk +xadd +xtrim) " +
		"(~events.stream:* +xreadgroup +xack +xautoclaim +xpending +xinfo|groups +xinfo|stream +xinfo|consumers " +
		"+xgroup|create +xgroup|createconsumer +xgroup|delconsumer +xrange)"
	if got := streamsvalkey.ACLSelectors("helpdesk"); got != want {
		t.Errorf("ACLSelectors = %q\nwant %q", got, want)
	}
}
