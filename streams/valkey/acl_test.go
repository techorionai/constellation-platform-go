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

// U25: an app name with ACL syntax is not accepted.
func TestU25InvalidApp(t *testing.T) {
	for _, app := range []string{"x) (~* +@all", "a b", "", "Helpdesk", "a:b"} {
		for name, fn := range map[string]func(){
			"ACLSelectors":  func() { streamsvalkey.ACLSelectors(app) },
			"NewPublisher":  func() { streamsvalkey.NewPublisher(nil, app) },
			"NewTrimPurger": func() { streamsvalkey.NewTrimPurger(nil, app) },
		} {
			func() {
				defer func() {
					if recover() == nil {
						t.Errorf("%s(%q) did not panic", name, app)
					}
				}()
				fn()
			}()
		}
	}
}
