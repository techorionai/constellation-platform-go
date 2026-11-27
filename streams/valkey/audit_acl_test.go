package valkey_test

import (
	"strings"
	"testing"

	"github.com/techorionai/constellation-platform-go/streams"
	streamsvalkey "github.com/techorionai/constellation-platform-go/streams/valkey"
)

// U11: the stream keys of the families.
func TestU11KeyFor(t *testing.T) {
	cases := map[string]string{
		streams.KeyFor(streams.Audit, "helpdesk"):  "audit.stream:helpdesk",
		streams.KeyFor(streams.Events, "helpdesk"): "events.stream:helpdesk",
		streams.StreamKey("helpdesk"):              "events.stream:helpdesk",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("key = %q, want %q", got, want)
		}
	}
	var zero streams.Family
	if zero != streams.Events {
		t.Error("the zero family is not Events")
	}
}

// U12: the text of the audit ACL selectors.
func TestU12AuditSelectors(t *testing.T) {
	wantProducer := "(~audit.stream:helpdesk +xadd +xtrim)"
	wantReader := "(~audit.stream:* +xreadgroup +xack +xautoclaim +xpending +xinfo|groups +xinfo|stream " +
		"+xinfo|consumers +xgroup|create +xgroup|createconsumer +xgroup|delconsumer +xrange) " +
		"(~audit.apps +sscan)"
	if got := streamsvalkey.AuditProducerSelectors("helpdesk"); got != wantProducer {
		t.Errorf("AuditProducerSelectors = %q\nwant %q", got, wantProducer)
	}
	if got := streamsvalkey.AuditReaderSelectors(); got != wantReader {
		t.Errorf("AuditReaderSelectors = %q\nwant %q", got, wantReader)
	}
	// ACLSelectors keeps the text of v0.6.0.
	wantEvents := "(~events.stream:helpdesk +xadd +xtrim) " +
		"(~events.stream:* +xreadgroup +xack +xautoclaim +xpending +xinfo|groups +xinfo|stream +xinfo|consumers " +
		"+xgroup|create +xgroup|createconsumer +xgroup|delconsumer +xrange)"
	if got := streamsvalkey.ACLSelectors("helpdesk"); got != wantEvents {
		t.Errorf("ACLSelectors = %q", got)
	}
	for s, want := range map[string]int{wantProducer: 1, wantReader: 2} {
		// Each selector has one ")" at its end.
		if n := strings.Count(s, ")"); n != want {
			t.Errorf("selector text has %d ')' characters, want %d: %q", n, want, s)
		}
		for _, bad := range []string{"audit.streams", "+sadd", "+srem"} {
			if strings.Contains(s, bad) {
				t.Errorf("selector text has %q: %q", bad, s)
			}
		}
	}
}

// U13: an invalid app or family panics.
func TestU13Panics(t *testing.T) {
	for name, fn := range map[string]func(){
		"AuditProducerSelectors": func() { streamsvalkey.AuditProducerSelectors("Bad") },
		"NewPublisher audit":     func() { streamsvalkey.NewPublisher(nil, "Bad", streamsvalkey.WithFamily(streams.Audit)) },
		"NewTrimPurger audit":    func() { streamsvalkey.NewTrimPurger(nil, "Bad", streamsvalkey.WithFamily(streams.Audit)) },
		"WithFamily":             func() { streamsvalkey.WithFamily(streams.Family(7)) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s did not panic", name)
				}
			}()
			fn()
		}()
	}
}

func TestNewGroupFamily(t *testing.T) {
	if _, err := streamsvalkey.NewGroup(nil, streamsvalkey.GroupConfig{App: "audit", Stream: "helpdesk", Group: "audit", Family: 9}); err == nil {
		t.Error("an unknown family gave no error")
	}
	if _, err := streamsvalkey.NewGroup(nil, streamsvalkey.GroupConfig{App: "audit", Stream: "helpdesk", Group: "audit", Family: streams.Audit}); err != nil {
		t.Errorf("audit family: %v", err)
	}
}

// U14: an invalid app gives an error and no command. A nil client panics if the
// function sends a command.
func TestU14RegisterInvalid(t *testing.T) {
	if err := streamsvalkey.RegisterAuditApp(t.Context(), nil, "Bad-App"); err == nil {
		t.Error("RegisterAuditApp gave no error")
	}
	if err := streamsvalkey.UnregisterAuditApp(t.Context(), nil, "Bad-App"); err == nil {
		t.Error("UnregisterAuditApp gave no error")
	}
}
