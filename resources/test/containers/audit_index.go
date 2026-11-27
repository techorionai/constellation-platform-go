package containers

import (
	"fmt"
	"slices"
	"testing"

	valkeygo "github.com/valkey-io/valkey-go"

	"github.com/techorionai/constellation-platform-go/streams"
)

// AuditIndexMembers returns the sorted members of the set audit.streams. Use an
// admin client. It fails the test on an error.
func AuditIndexMembers(t testing.TB, admin valkeygo.Client) []string {
	t.Helper()
	got, err := admin.Do(t.Context(), admin.B().Smembers().Key(streams.AuditIndexKey).Build()).AsStrSlice()
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	return got
}

// SeedAuditIndex replaces the set audit.streams. The new set has the forged
// names f000001 and up, which pass the app name pattern and have no stream, and
// the apps, each with an audit stream. Use an admin client. It fails the test on
// an error.
func SeedAuditIndex(t testing.TB, admin valkeygo.Client, forged int, apps ...string) {
	t.Helper()
	run := func(cmd ...string) {
		t.Helper()
		if err := admin.Do(t.Context(), admin.B().Arbitrary(cmd[0]).Args(cmd[1:]...).Build()).Error(); err != nil {
			t.Fatal(err)
		}
	}
	run("DEL", streams.AuditIndexKey)
	for _, a := range apps {
		run("XADD", streams.KeyFor(streams.Audit, a), "*", "event", "{}")
		run("SADD", streams.AuditIndexKey, a)
	}
	for start := 0; start < forged; start += 1000 {
		cmd := []string{"SADD", streams.AuditIndexKey}
		for i := start; i < min(start+1000, forged); i++ {
			cmd = append(cmd, fmt.Sprintf("f%06d", i+1))
		}
		run(cmd...)
	}
}
