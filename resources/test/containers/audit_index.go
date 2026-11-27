package containers

import (
	"fmt"
	"slices"
	"testing"

	valkeygo "github.com/valkey-io/valkey-go"

	"github.com/techorionai/constellation-platform-go/streams"
	streamsvalkey "github.com/techorionai/constellation-platform-go/streams/valkey"
)

// AuditIndexMembers returns the sorted members of the trusted set audit.apps.
// Use an admin client. It fails the test on an error.
func AuditIndexMembers(t testing.TB, admin valkeygo.Client) []string {
	t.Helper()
	got, err := admin.Do(t.Context(), admin.B().Smembers().Key(streams.AuditAppsKey).Build()).AsStrSlice()
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	return got
}

// RegisterAuditApps registers each app in the trusted set audit.apps with
// streams/valkey.RegisterAuditApp. Use an admin client. It fails the test on an
// error.
func RegisterAuditApps(t testing.TB, admin valkeygo.Client, apps ...string) {
	t.Helper()
	for _, a := range apps {
		if err := streamsvalkey.RegisterAuditApp(t.Context(), admin, a); err != nil {
			t.Fatal(err)
		}
	}
}

// SeedAuditIndex replaces the old set audit.streams. The new set has the forged
// names f000001 and up, which pass the app name pattern and have no stream, and
// the apps, each with an audit stream. It writes only audit.streams and the app
// streams, and it does not write audit.apps. Use [RegisterAuditApps] for that.
// Use an admin client. It fails the test on an error.
func SeedAuditIndex(t testing.TB, admin valkeygo.Client, forged int, apps ...string) {
	t.Helper()
	run := func(cmd ...string) {
		t.Helper()
		if err := admin.Do(t.Context(), admin.B().Arbitrary(cmd[0]).Args(cmd[1:]...).Build()).Error(); err != nil {
			t.Fatal(err)
		}
	}
	run("DEL", streams.AuditIndexKey) //nolint:staticcheck // the helper seeds the old set
	for _, a := range apps {
		run("XADD", streams.KeyFor(streams.Audit, a), "*", "event", "{}")
		run("SADD", streams.AuditIndexKey, a) //nolint:staticcheck // the helper seeds the old set
	}
	for start := 0; start < forged; start += 1000 {
		cmd := []string{"SADD", streams.AuditIndexKey} //nolint:staticcheck // the helper seeds the old set
		for i := start; i < min(start+1000, forged); i++ {
			cmd = append(cmd, fmt.Sprintf("f%06d", i+1))
		}
		run(cmd...)
	}
}
