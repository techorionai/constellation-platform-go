//go:build integration

package valkey_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	valkeygo "github.com/valkey-io/valkey-go"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/techorionai/constellation-platform-go/resources/test/containers"
	"github.com/techorionai/constellation-platform-go/retention"
	"github.com/techorionai/constellation-platform-go/streams"
	streamsvalkey "github.com/techorionai/constellation-platform-go/streams/valkey"
)

// sampleEvent returns a CloudEvent that agrees with the stream of app.
func sampleEvent(app string) string {
	return `{"specversion":"1.0","id":"1","source":"/` + app + `","type":"` + app + `.user.created","data":{}}`
}

func xadd(t *testing.T, c valkeygo.Client, app, id string) {
	t.Helper()
	if err := c.Do(t.Context(), c.B().Xadd().Key(streams.StreamKey(app)).Id(id).FieldValue().FieldValue("event", sampleEvent(app)).Build()).Error(); err != nil {
		t.Fatal(err)
	}
}

func pendingCount(t *testing.T, c valkeygo.Client, app, group string) int64 {
	t.Helper()
	arr, err := c.Do(t.Context(), c.B().Xpending().Key(streams.StreamKey(app)).Group(group).Build()).ToArray()
	if err != nil {
		t.Fatal(err)
	}
	n, err := arr[0].AsInt64()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func newGroup(t *testing.T, c valkeygo.Client, cfg streamsvalkey.GroupConfig) *streamsvalkey.Group {
	t.Helper()
	g, err := streamsvalkey.NewGroup(c, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	return g
}

func consumerNames(t *testing.T, c valkeygo.Client, app, group string) []string {
	t.Helper()
	arr, err := c.Do(t.Context(), c.B().Arbitrary("XINFO", "CONSUMERS").Keys(streams.StreamKey(app)).Args(group).Build()).ToArray()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, item := range arr {
		m, err := item.AsMap()
		if err != nil {
			t.Fatal(err)
		}
		n := m["name"]
		s, err := n.ToString()
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, s)
	}
	return names
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func aclClient(t *testing.T, vk *containers.ValkeyInstance, prefix string) valkeygo.Client {
	t.Helper()
	user, password := vk.NewACLUser(t, prefix, containers.ValkeyACLEvents())
	c, err := valkeygo.NewClient(valkeygo.ClientOption{
		InitAddress: []string{vk.Address()}, Username: user, Password: password.Reveal(), DisableCache: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestIntegration(t *testing.T) {
	vk := containers.Valkey(t)
	admin := vk.Admin(t)
	ctx := t.Context()

	// I8: create, read, ack, and create again.
	t.Run("I8_group_basics", func(t *testing.T) {
		g := newGroup(t, admin, streamsvalkey.GroupConfig{App: "billing", Stream: "i8", Group: "billing"})
		if err := g.Ensure(ctx); err != nil {
			t.Fatalf("second Ensure: %v", err)
		}
		for i := 1; i <= 3; i++ {
			xadd(t, admin, "i8", fmt.Sprintf("%d-0", time.Now().UnixMilli()+int64(i)))
		}
		msgs, err := g.Read(ctx, 10, time.Second)
		if err != nil || len(msgs) != 3 {
			t.Fatalf("Read = %d messages, %v", len(msgs), err)
		}
		ids := make([]string, len(msgs))
		for i, m := range msgs {
			ids[i] = m.ID
			if m.Deliveries != 1 || string(m.Event) != sampleEvent("i8") {
				t.Errorf("message %d = %+v", i, m)
			}
		}
		if n := pendingCount(t, admin, "i8", "billing"); n != 3 {
			t.Errorf("pending = %d, want 3", n)
		}
		if err := g.Ack(ctx, ids...); err != nil {
			t.Fatal(err)
		}
		if n := pendingCount(t, admin, "i8", "billing"); n != 0 {
			t.Errorf("pending after ack = %d, want 0", n)
		}
		if msgs, err := g.Read(ctx, 10, 100*time.Millisecond); err != nil || len(msgs) != 0 {
			t.Errorf("empty Read = %d, %v", len(msgs), err)
		}
	})

	// I9: a second consumer claims a message that the first did not ack.
	t.Run("I9_claim", func(t *testing.T) {
		a := newGroup(t, admin, streamsvalkey.GroupConfig{App: "billing", Stream: "i9", Group: "billing", Consumer: "a"})
		b := newGroup(t, admin, streamsvalkey.GroupConfig{App: "billing", Stream: "i9", Group: "billing", Consumer: "b"})
		xadd(t, admin, "i9", "*")
		if msgs, err := a.Read(ctx, 10, time.Second); err != nil || len(msgs) != 1 {
			t.Fatalf("Read = %d, %v", len(msgs), err)
		}
		if msgs, _, err := b.Claim(ctx, time.Hour, 10); err != nil || len(msgs) != 0 {
			t.Fatalf("early Claim = %d, %v", len(msgs), err)
		}
		time.Sleep(300 * time.Millisecond)
		msgs, deleted, err := b.Claim(ctx, 200*time.Millisecond, 10)
		if err != nil || len(msgs) != 1 || len(deleted) != 0 {
			t.Fatalf("Claim = %d, %v, %v", len(msgs), deleted, err)
		}
		if msgs[0].Deliveries != 2 {
			t.Errorf("Deliveries = %d, want 2", msgs[0].Deliveries)
		}
		if err := b.Ack(ctx, msgs[0].ID); err != nil {
			t.Fatal(err)
		}
		if n := pendingCount(t, admin, "i9", "billing"); n != 0 {
			t.Errorf("pending = %d, want 0", n)
		}
	})

	// I10: the lag of a group.
	t.Run("I10_lag", func(t *testing.T) {
		g := newGroup(t, admin, streamsvalkey.GroupConfig{App: "billing", Stream: "i10a", Group: "billing", StartID: "0"})
		lag, err := streamsvalkey.Lag(ctx, admin, "i10a", "billing")
		if err != nil || lag != 0 {
			t.Fatalf("empty lag = %v, %v", lag, err)
		}
		old := time.Now().Add(-60 * time.Second).UnixMilli()
		xadd(t, admin, "i10a", fmt.Sprintf("%d-0", old))
		// Undelivered entry of age 60 s.
		lag, err = streamsvalkey.Lag(ctx, admin, "i10a", "billing")
		if err != nil || lag < 59*time.Second || lag > 70*time.Second {
			t.Fatalf("undelivered lag = %v, %v", lag, err)
		}
		// Pending entry of age 60 s.
		if msgs, err := g.Read(ctx, 10, time.Second); err != nil || len(msgs) != 1 {
			t.Fatalf("Read = %d, %v", len(msgs), err)
		}
		lag, err = streamsvalkey.Lag(ctx, admin, "i10a", "billing")
		if err != nil || lag < 59*time.Second || lag > 70*time.Second {
			t.Fatalf("pending lag = %v, %v", lag, err)
		}
		// After the ack, no lag.
		msgs, _, err := g.Claim(ctx, 0, 10)
		if err != nil || len(msgs) != 1 {
			t.Fatalf("Claim = %d, %v", len(msgs), err)
		}
		if err := g.Ack(ctx, msgs[0].ID); err != nil {
			t.Fatal(err)
		}
		if lag, err = streamsvalkey.Lag(ctx, admin, "i10a", "billing"); err != nil || lag != 0 {
			t.Fatalf("lag after ack = %v, %v", lag, err)
		}
	})

	// I11: the trim purger and the lost messages.
	t.Run("I11_trim", func(t *testing.T) {
		a := newGroup(t, admin, streamsvalkey.GroupConfig{App: "billing", Stream: "i11", Group: "billing", StartID: "0", Consumer: "a"})
		old := time.Now().Add(-10 * 24 * time.Hour).UnixMilli()
		for i := range 5 {
			xadd(t, admin, "i11", fmt.Sprintf("%d-0", old+int64(i)))
		}
		xadd(t, admin, "i11", "*")
		msgs, err := a.Read(ctx, 3, time.Second)
		if err != nil || len(msgs) != 3 {
			t.Fatalf("Read = %d, %v", len(msgs), err)
		}
		p := streamsvalkey.NewTrimPurger(admin, "i11")
		n, err := p.Purge(ctx, retention.PurgeParams{Before: time.Now().Add(-72 * time.Hour), Limit: 1})
		if err != nil || n != 5 {
			t.Fatalf("first Purge = %d, %v, want 5", n, err)
		}
		if n, err = p.Purge(ctx, retention.PurgeParams{Before: time.Now().Add(-72 * time.Hour), Limit: 1}); err != nil || n != 0 {
			t.Fatalf("second Purge = %d, %v, want 0", n, err)
		}
		length, err := admin.Do(ctx, admin.B().Xlen().Key(streams.StreamKey("i11")).Build()).AsInt64()
		if err != nil || length != 1 {
			t.Fatalf("XLEN = %d, %v, want 1", length, err)
		}

		reader := sdkmetric.NewManualReader()
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		defer func() { _ = mp.Shutdown(context.Background()) }()
		b := newGroup(t, admin, streamsvalkey.GroupConfig{App: "billing", Stream: "i11", Group: "billing", Consumer: "b"})
		c, err := streams.NewConsumer(b, func(context.Context, streams.Message) error { return nil },
			streams.ConsumerOptions{Block: 100 * time.Millisecond, ClaimIdle: 100 * time.Millisecond, MeterProvider: mp})
		if err != nil {
			t.Fatal(err)
		}
		cctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); _ = c.Run(cctx) }()
		defer func() { cancel(); <-done }()
		waitFor(t, "the lost counter", 10*time.Second, func() bool {
			var rm metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &rm); err != nil {
				return false
			}
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					if m.Name == "constellation.events.consumer.lost" {
						for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
							if dp.Value == 3 {
								return true
							}
						}
					}
				}
			}
			return false
		})
	})

	// I12: a user with another root pattern cannot write to the stream of helpdesk.
	t.Run("I12_acl_events_user", func(t *testing.T) {
		c := aclClient(t, vk, "events")
		for _, cmd := range [][]string{
			{"XADD", "events.stream:helpdesk", "*", "event", "x"},
			{"DEL", "events.stream:helpdesk"},
		} {
			if err := doArbitrary(c, cmd); !isNoPerm(err) {
				t.Errorf("%v: err = %v, want NOPERM", cmd, err)
			}
		}
	})

	// I13: the ACL selectors of a service user.
	t.Run("I13_acl_service_user", func(t *testing.T) {
		c := aclClient(t, vk, "helpdesk")
		if err := admin.Do(ctx, admin.B().XgroupCreate().Key("events.stream:identity").Group("helpdesk").Id("0").Mkstream().Build()).Error(); err != nil {
			t.Fatal(err)
		}
		allowed := [][]string{
			{"XADD", "events.stream:helpdesk", "*", "event", "x"},
			{"XREADGROUP", "GROUP", "helpdesk", "c1", "COUNT", "1", "STREAMS", "events.stream:identity", ">"},
			{"XACK", "events.stream:identity", "helpdesk", "0-1"},
			{"XINFO", "CONSUMERS", "events.stream:identity", "helpdesk"},
			{"XGROUP", "DELCONSUMER", "events.stream:identity", "helpdesk", "c1"},
		}
		for _, cmd := range allowed {
			if err := doArbitrary(c, cmd); err != nil && !valkeygo.IsValkeyNil(err) {
				t.Errorf("%v: err = %v, want allowed", cmd, err)
			}
		}
		refused := [][]string{
			{"XADD", "events.stream:identity", "*", "event", "x"},
			{"XTRIM", "events.stream:identity", "MAXLEN", "0"},
			{"DEL", "events.stream:identity"},
		}
		for _, cmd := range refused {
			if err := doArbitrary(c, cmd); !isNoPerm(err) {
				t.Errorf("%v: err = %v, want NOPERM", cmd, err)
			}
		}
	})

	// I14: a context cancel stops a blocked read.
	t.Run("I14_cancel", func(t *testing.T) {
		g := newGroup(t, admin, streamsvalkey.GroupConfig{App: "billing", Stream: "i14", Group: "billing"})
		c, err := streams.NewConsumer(g, func(context.Context, streams.Message) error { return nil }, streams.ConsumerOptions{Block: 30 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		cctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); _ = c.Run(cctx) }()
		time.Sleep(500 * time.Millisecond)
		start := time.Now()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return")
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("Run returned after %v, want less than 1s", d)
		}
	})

	// I16: a poison message is dropped after MaxDeliveries.
	t.Run("I16_poison", func(t *testing.T) {
		reader := sdkmetric.NewManualReader()
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		defer func() { _ = mp.Shutdown(context.Background()) }()
		g := newGroup(t, admin, streamsvalkey.GroupConfig{App: "billing", Stream: "i16", Group: "billing"})
		xadd(t, admin, "i16", "*")
		var calls atomic.Int32
		c, err := streams.NewConsumer(g, func(context.Context, streams.Message) error {
			calls.Add(1)
			return errors.New("always fails")
		}, streams.ConsumerOptions{Block: 100 * time.Millisecond, ClaimIdle: 100 * time.Millisecond, MaxDeliveries: 3, MeterProvider: mp})
		if err != nil {
			t.Fatal(err)
		}
		cctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); _ = c.Run(cctx) }()
		defer func() { cancel(); <-done }()
		waitFor(t, "the message is dropped", 20*time.Second, func() bool {
			return calls.Load() >= 3 && pendingCount(t, admin, "i16", "billing") == 0
		})
		time.Sleep(300 * time.Millisecond)
		if calls.Load() != 3 {
			t.Errorf("handler calls = %d, want 3", calls.Load())
		}
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatal(err)
		}
		var dropped int64
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "constellation.events.consumer.handled" {
					continue
				}
				for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
					if v, _ := dp.Attributes.Value(attribute.Key("outcome")); v.AsString() == "dropped" {
						dropped += dp.Value
					}
				}
			}
		}
		if dropped != 1 {
			t.Errorf("dropped = %d, want 1", dropped)
		}
	})

	// I17: the claim cursor claims each entry one time.
	t.Run("I17_cursor", func(t *testing.T) {
		a := newGroup(t, admin, streamsvalkey.GroupConfig{App: "billing", Stream: "i17", Group: "billing", StartID: "0", Consumer: "a"})
		b := newGroup(t, admin, streamsvalkey.GroupConfig{App: "billing", Stream: "i17", Group: "billing", Consumer: "b"})
		now := time.Now().UnixMilli()
		cmds := make(valkeygo.Commands, 0, 1000)
		for i := range 1000 {
			cmds = append(cmds, admin.B().Xadd().Key(streams.StreamKey("i17")).Id(fmt.Sprintf("%d-0", now-100000+int64(i))).
				FieldValue().FieldValue("event", sampleEvent("i17")).Build())
		}
		for _, r := range admin.DoMulti(ctx, cmds...) {
			if err := r.Error(); err != nil {
				t.Fatal(err)
			}
		}
		if msgs, err := a.Read(ctx, 1000, time.Second); err != nil || len(msgs) != 1000 {
			t.Fatalf("Read = %d, %v", len(msgs), err)
		}
		time.Sleep(time.Second)
		seen := map[string]int{}
		for range 100 {
			msgs, _, err := b.Claim(ctx, 800*time.Millisecond, 10)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range msgs {
				seen[m.ID]++
				if m.Deliveries != 2 {
					t.Fatalf("Deliveries = %d, want 2", m.Deliveries)
				}
			}
		}
		if len(seen) != 1000 {
			t.Fatalf("claimed %d different entries, want 1000", len(seen))
		}
		for id, n := range seen {
			if n != 1 {
				t.Fatalf("entry %s claimed %d times", id, n)
			}
		}
	})

	// I18: Groups and Lag for missing streams and groups.
	t.Run("I18_groups", func(t *testing.T) {
		names, err := streamsvalkey.Groups(ctx, admin, "i18none")
		if err != nil || len(names) != 0 {
			t.Fatalf("missing stream: %v, %v", names, err)
		}
		newGroup(t, admin, streamsvalkey.GroupConfig{App: "billing", Stream: "i18", Group: "billing.b"})
		newGroup(t, admin, streamsvalkey.GroupConfig{App: "billing", Stream: "i18", Group: "billing.a"})
		names, err = streamsvalkey.Groups(ctx, admin, "i18")
		if err != nil || strings.Join(names, ",") != "billing.a,billing.b" {
			t.Fatalf("Groups = %v, %v", names, err)
		}
		if _, err := streamsvalkey.Lag(ctx, admin, "i18", "missing"); !errors.Is(err, streamsvalkey.ErrNoGroup) {
			t.Errorf("Lag with a missing group: err = %v, want ErrNoGroup", err)
		}
		if _, err := streamsvalkey.Lag(ctx, admin, "i18none", "missing"); !errors.Is(err, streamsvalkey.ErrNoGroup) {
			t.Errorf("Lag with a missing stream: err = %v, want ErrNoGroup", err)
		}
	})

	// I19: the cleanup of consumers.
	t.Run("I19_cleanup", func(t *testing.T) {
		cfg := func(name string) streamsvalkey.GroupConfig {
			return streamsvalkey.GroupConfig{App: "billing", Stream: "i19", Group: "billing", Consumer: name}
		}
		live := newGroup(t, admin, cfg("live"))
		a := newGroup(t, admin, cfg("a"))
		b := newGroup(t, admin, cfg("b"))
		if _, err := a.Read(ctx, 1, 100*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		if err := a.Leave(ctx); err != nil {
			t.Fatal(err)
		}
		xadd(t, admin, "i19", "*")
		msgs, err := b.Read(ctx, 1, time.Second)
		if err != nil || len(msgs) != 1 {
			t.Fatalf("Read = %d, %v", len(msgs), err)
		}
		if err := b.Leave(ctx); err != nil {
			t.Fatal(err)
		}
		names := consumerNames(t, admin, "i19", "billing")
		if strings.Join(sorted(names), ",") != "b" {
			t.Fatalf("consumers after the stops = %v, want [b]", names)
		}
		if err := live.Ack(ctx, msgs[0].ID); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		if _, err := live.Read(ctx, 1, 100*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		n, err := live.Cleanup(ctx, 100*time.Millisecond)
		if err != nil || n != 1 {
			t.Fatalf("Cleanup = %d, %v, want 1", n, err)
		}
		if names := consumerNames(t, admin, "i19", "billing"); strings.Join(names, ",") != "live" {
			t.Fatalf("consumers after the cleanup = %v, want [live]", names)
		}
	})

	t.Run("invalid_group", func(t *testing.T) {
		_, err := streamsvalkey.NewGroup(admin, streamsvalkey.GroupConfig{App: "helpdesk", Stream: "identity", Group: "identity"})
		if !errors.Is(err, streams.ErrInvalidGroup) {
			t.Fatalf("err = %v, want ErrInvalidGroup", err)
		}
	})

}

func doArbitrary(c valkeygo.Client, cmd []string) error {
	return c.Do(context.Background(), c.B().Arbitrary(cmd[0]).Args(cmd[1:]...).Build()).Error()
}

func isNoPerm(err error) bool {
	return err != nil && strings.Contains(err.Error(), "NOPERM")
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func auditClient(t *testing.T, vk *containers.ValkeyInstance, prefix string, opts ...containers.ACLOption) valkeygo.Client {
	t.Helper()
	user, password := vk.NewACLUser(t, prefix, opts...)
	c, err := valkeygo.NewClient(valkeygo.ClientOption{
		InitAddress: []string{vk.Address()}, Username: user, Password: password.Reveal(), DisableCache: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// auditReaderWith makes a reader with the literal selectors of text, besides the
// event selectors that NewACLUser adds.
func auditReaderWith(t *testing.T, vk *containers.ValkeyInstance, admin valkeygo.Client, text string) valkeygo.Client {
	t.Helper()
	user, password := vk.NewACLUser(t, "auditold", containers.ValkeyACLEvents())
	args := append([]string{"ACL", "SETUSER", user}, splitSelectors(text)...)
	if err := doArbitrary(admin, args); err != nil {
		t.Fatal(err)
	}
	reader, err := valkeygo.NewClient(valkeygo.ClientOption{
		InitAddress: []string{vk.Address()}, Username: user, Password: password.Reveal(), DisableCache: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reader.Close)
	return reader
}

// splitSelectors splits the text of ACL selectors into one argument for each selector.
func splitSelectors(s string) []string {
	var out []string
	for {
		i := strings.Index(s, ")")
		if i < 0 {
			return out
		}
		out = append(out, strings.TrimSpace(s[:i+1]))
		s = s[i+1:]
	}
}

// auditStreamSelectors is the stream part of the reader selectors of v0.8.0 and v0.9.0.
const auditStreamSelectors = "(~audit.stream:* +xreadgroup +xack +xautoclaim +xpending +xinfo|groups +xinfo|stream " +
	"+xinfo|consumers +xgroup|create +xgroup|createconsumer +xgroup|delconsumer +xrange)"

// xinfoGroupsCalls returns the number of XINFO GROUPS commands that the server ran.
func xinfoGroupsCalls(t *testing.T, admin valkeygo.Client) int {
	t.Helper()
	info, err := admin.Do(t.Context(), admin.B().Info().Section("commandstats").Build()).ToString()
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(info, "\n") {
		if rest, ok := strings.CutPrefix(line, "cmdstat_xinfo|groups:calls="); ok {
			n, _, _ := strings.Cut(rest, ",")
			calls, err := strconv.Atoi(n)
			if err != nil {
				t.Fatal(err)
			}
			return calls
		}
	}
	return 0
}

// netOutputBytes returns the number of bytes that the server sent to all clients.
func netOutputBytes(t *testing.T, admin valkeygo.Client) int {
	t.Helper()
	info, err := admin.Do(t.Context(), admin.B().Info().Section("stats").Build()).ToString()
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(info, "\n") {
		if rest, ok := strings.CutPrefix(line, "total_net_output_bytes:"); ok {
			n, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatal("total_net_output_bytes not found")
	return 0
}

func TestIntegrationAuditApps(t *testing.T) {
	vk := containers.Valkey(t)
	admin := vk.Admin(t)
	newReader := func(t *testing.T) valkeygo.Client {
		return auditClient(t, vk, "audit", containers.ValkeyACLEvents(), containers.ValkeyACLAuditReader())
	}
	reset := func(t *testing.T) {
		t.Helper()
		if err := doArbitrary(admin, []string{"DEL", streams.AuditAppsKey, streams.AuditIndexKey}); err != nil { //nolint:staticcheck // the test uses the old set
			t.Fatal(err)
		}
	}
	addStream := func(t *testing.T, app string) {
		t.Helper()
		if err := doArbitrary(admin, []string{"XADD", streams.KeyFor(streams.Audit, app), "*", "event", "{}"}); err != nil {
			t.Fatal(err)
		}
	}
	// registerMany registers n apps m000001 and up, each with a stream.
	registerMany := func(t *testing.T, n int) {
		t.Helper()
		reset(t)
		for start := 0; start < n; start += 500 {
			add := []string{"SADD", streams.AuditAppsKey}
			for i := start; i < min(start+500, n); i++ {
				app := fmt.Sprintf("m%06d", i+1)
				addStream(t, app)
				add = append(add, app)
			}
			if err := doArbitrary(admin, add); err != nil {
				t.Fatal(err)
			}
		}
	}

	// I7c: regression test of finding R5. Forged names in audit.streams give no
	// XINFO GROUPS command.
	t.Run("I7c_forged_names_give_no_load", func(t *testing.T) {
		reset(t)
		containers.SeedAuditIndex(t, admin, 100000, "helpdesk", "crm")
		containers.RegisterAuditApps(t, admin, "helpdesk", "crm")
		reader := newReader(t)
		before := xinfoGroupsCalls(t, admin)
		apps, err := streamsvalkey.AuditApps(t.Context(), reader)
		if err != nil || !slices.Equal(apps, []string{"crm", "helpdesk"}) {
			t.Fatalf("AuditApps = %v, err = %v", apps, err)
		}
		if n := xinfoGroupsCalls(t, admin) - before; n != 2 {
			t.Errorf("%d XINFO GROUPS commands, want 2", n)
		}
	})

	// I7h: regression test of finding R6. A large entry of a producer does not
	// come back in the stream check.
	t.Run("I7h_large_entry_not_returned", func(t *testing.T) {
		reset(t)
		const size = 5 << 20
		for _, app := range []string{"helpdesk", "crm"} {
			big := strings.Repeat("x", size)
			if err := doArbitrary(admin, []string{"XADD", streams.KeyFor(streams.Audit, app), "*", "event", big}); err != nil {
				t.Fatal(err)
			}
		}
		containers.RegisterAuditApps(t, admin, "helpdesk", "crm")
		reader := newReader(t)
		before := netOutputBytes(t, admin)
		apps, err := streamsvalkey.AuditApps(t.Context(), reader)
		if err != nil || !slices.Equal(apps, []string{"crm", "helpdesk"}) {
			t.Fatalf("AuditApps = %v, err = %v", apps, err)
		}
		// XINFO STREAM gives the first and the last entry, more than 20 MB for 2 streams.
		if n := netOutputBytes(t, admin) - before; n > 1<<20 {
			t.Errorf("the server sent %d bytes for the check, want less than 1 MiB", n)
		}
	})

	// I7f: the scan limit counts the members that the scan reads.
	t.Run("I7f_scan_limit", func(t *testing.T) {
		registerMany(t, 700)
		reader := newReader(t)
		apps, err := streamsvalkey.AuditAppsWith(t.Context(), reader, streamsvalkey.AuditHooks{MemberLimit: 600})
		if !errors.Is(err, streamsvalkey.ErrTooManyAuditApps) {
			t.Fatalf("err = %v, want ErrTooManyAuditApps", err)
		}
		if len(apps) != 600 {
			t.Errorf("%d apps, want 600", len(apps))
		}
		if got := containers.AuditIndexMembers(t, admin); len(got) != 700 {
			t.Errorf("audit.apps has %d members, want 700", len(got))
		}
	})

	// I7g: a set with as many members as the limit is not truncated.
	t.Run("I7g_at_limit", func(t *testing.T) {
		registerMany(t, 600)
		reader := newReader(t)
		apps, err := streamsvalkey.AuditAppsWith(t.Context(), reader, streamsvalkey.AuditHooks{MemberLimit: 600})
		if err != nil || len(apps) != 600 {
			t.Fatalf("%d apps, err = %v, want 600 apps and no error", len(apps), err)
		}
	})

	// I10: register and unregister an app.
	t.Run("I10_register_unregister", func(t *testing.T) {
		reset(t)
		member := func() bool {
			ok, err := admin.Do(t.Context(), admin.B().Sismember().Key(streams.AuditAppsKey).Member("newapp").Build()).AsBool()
			if err != nil {
				t.Fatal(err)
			}
			return ok
		}
		if err := streamsvalkey.RegisterAuditApp(t.Context(), admin, "newapp"); err != nil || !member() {
			t.Fatalf("after RegisterAuditApp: member = %v, err = %v", member(), err)
		}
		if err := streamsvalkey.UnregisterAuditApp(t.Context(), admin, "newapp"); err != nil || member() {
			t.Fatalf("after UnregisterAuditApp: member = %v, err = %v", member(), err)
		}
		if err := streamsvalkey.RegisterAuditApp(t.Context(), admin, "Bad-App"); err == nil {
			t.Error("RegisterAuditApp with Bad-App gave no error")
		}
		if got := containers.AuditIndexMembers(t, admin); len(got) != 0 {
			t.Errorf("audit.apps = %v, want no member", got)
		}
	})

	// I11: a producer that is not registered is not found.
	t.Run("I11_unregistered_not_found", func(t *testing.T) {
		reset(t)
		producer := auditClient(t, vk, "newapp", containers.ValkeyACLEvents(), containers.ValkeyACLAuditProducer())
		if err := streamsvalkey.NewPublisher(producer, "newapp", streamsvalkey.WithFamily(streams.Audit)).
			Publish(t.Context(), [][]byte{[]byte(sampleEvent("newapp"))}); err != nil {
			t.Fatal(err)
		}
		reader := newReader(t)
		apps, err := streamsvalkey.AuditApps(t.Context(), reader)
		if err != nil || len(apps) != 0 {
			t.Fatalf("before registration: AuditApps = %v, err = %v", apps, err)
		}
		containers.RegisterAuditApps(t, admin, "newapp")
		apps, err = streamsvalkey.AuditApps(t.Context(), reader)
		if err != nil || !slices.Equal(apps, []string{"newapp"}) {
			t.Fatalf("after registration: AuditApps = %v, err = %v", apps, err)
		}
	})

	// I12: a reader with the selectors of v0.8.0 gets a visible error.
	t.Run("I12_old_reader_selectors", func(t *testing.T) {
		reset(t)
		containers.SeedAuditIndex(t, admin, 0, "helpdesk")
		containers.RegisterAuditApps(t, admin, "helpdesk")
		reader := auditReaderWith(t, vk, admin, auditStreamSelectors+" (~audit.streams +sscan +srem)")
		apps, err := streamsvalkey.AuditApps(t.Context(), reader)
		if !isNoPerm(err) || !strings.Contains(err.Error(), "SSCAN audit.apps") || apps != nil {
			t.Fatalf("AuditApps = %v, err = %v, want a NOPERM error for SSCAN audit.apps", apps, err)
		}
		if got, err := admin.Do(t.Context(), admin.B().Smembers().Key(streams.AuditIndexKey).Build()).AsStrSlice(); err != nil || !slices.Equal(got, []string{"helpdesk"}) { //nolint:staticcheck // the test uses the old set
			t.Errorf("audit.streams = %v, err = %v", got, err)
		}
	})

	// I13: a reader with the transition selectors of decision D12.
	t.Run("I13_transition_reader_selectors", func(t *testing.T) {
		reset(t)
		containers.SeedAuditIndex(t, admin, 0, "helpdesk")
		containers.RegisterAuditApps(t, admin, "helpdesk")
		reader := auditReaderWith(t, vk, admin, auditStreamSelectors+" (~audit.apps +sscan) (~audit.streams +sscan)")
		apps, err := streamsvalkey.AuditApps(t.Context(), reader)
		if err != nil || !slices.Equal(apps, []string{"helpdesk"}) {
			t.Fatalf("AuditApps = %v, err = %v", apps, err)
		}
		if err := doArbitrary(reader, []string{"SSCAN", "audit.streams", "0"}); err != nil {
			t.Errorf("SSCAN audit.streams: %v", err)
		}
		if err := doArbitrary(reader, []string{"SREM", "audit.streams", "x"}); !isNoPerm(err) {
			t.Errorf("SREM audit.streams: err = %v, want NOPERM", err)
		}
	})

	// I14: a set that does not exist gives an empty list.
	t.Run("I14_no_set", func(t *testing.T) {
		reset(t)
		addStream(t, "helpdesk")
		apps, err := streamsvalkey.AuditApps(t.Context(), newReader(t))
		if err != nil || len(apps) != 0 {
			t.Fatalf("AuditApps = %v, err = %v", apps, err)
		}
	})
}
