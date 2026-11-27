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

// auditOldReader makes a reader with the selectors of v0.7.0, with no +srem.
func auditOldReader(t *testing.T, vk *containers.ValkeyInstance, admin valkeygo.Client) valkeygo.Client {
	t.Helper()
	user, password := vk.NewACLUser(t, "auditold", containers.ValkeyACLEvents())
	if err := doArbitrary(admin, []string{"ACL", "SETUSER", user,
		"(~audit.stream:* +xinfo|stream +xrange)", "(~audit.streams +sscan)"}); err != nil {
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

// xinfoStreamCalls returns the number of XINFO STREAM commands that the server ran.
func xinfoStreamCalls(t *testing.T, admin valkeygo.Client) int {
	t.Helper()
	info, err := admin.Do(t.Context(), admin.B().Info().Section("commandstats").Build()).ToString()
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(info, "\n") {
		if rest, ok := strings.CutPrefix(line, "cmdstat_xinfo|stream:calls="); ok {
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

func TestIntegrationAuditApps(t *testing.T) {
	vk := containers.Valkey(t)
	admin := vk.Admin(t)

	// I7e: the second check finds an app that publishes between the first check and the SREM.
	t.Run("I7e_second_check", func(t *testing.T) {
		containers.SeedAuditIndex(t, admin, 0, "helpdesk")
		if err := doArbitrary(admin, []string{"SADD", streams.AuditIndexKey, "newapp"}); err != nil {
			t.Fatal(err)
		}
		producer := auditClient(t, vk, "newapp", containers.ValkeyACLEvents(), containers.ValkeyACLAuditProducer())
		reader := auditClient(t, vk, "audit", containers.ValkeyACLEvents(), containers.ValkeyACLAuditReader())
		apps, err := streamsvalkey.AuditAppsWith(t.Context(), reader, streamsvalkey.AuditHooks{
			BeforeSrem: func() {
				if err := doArbitrary(producer, []string{"XADD", "audit.stream:newapp", "*", "event", "{}"}); err != nil {
					t.Error(err)
				}
			},
		})
		if err != nil || !slices.Equal(apps, []string{"helpdesk", "newapp"}) {
			t.Fatalf("AuditApps = %v, err = %v", apps, err)
		}
	})

	// I7f: the scan limit counts the members that the scan reads. The scan deletes
	// each member that it read and that has no stream. The unread members stay.
	t.Run("I7f_scan_limit", func(t *testing.T) {
		const limit = 600
		containers.SeedAuditIndex(t, admin, 700, "helpdesk")
		reader := auditClient(t, vk, "audit", containers.ValkeyACLEvents(), containers.ValkeyACLAuditReader())
		apps, err := streamsvalkey.AuditAppsWith(t.Context(), reader, streamsvalkey.AuditHooks{MemberLimit: limit})
		if !errors.Is(err, streamsvalkey.ErrTooManyAuditApps) {
			t.Fatalf("err = %v, want ErrTooManyAuditApps", err)
		}
		left := containers.AuditIndexMembers(t, admin)
		if !slices.Contains(left, "helpdesk") {
			t.Fatalf("set = %d members, the app helpdesk is not in it", len(left))
		}
		// The scan read exactly 600 members. It deleted each one that has no stream.
		wantLeft := 701 - limit
		if len(apps) == 1 {
			wantLeft++
		}
		if len(apps) > 1 || len(apps) == 1 && apps[0] != "helpdesk" {
			t.Fatalf("apps = %v, want [helpdesk] or no app", apps)
		}
		if len(left) != wantLeft {
			t.Errorf("set has %d members, want %d", len(left), wantLeft)
		}
	})

	// I7g: a set with fewer members than the limit is not truncated.
	t.Run("I7g_below_limit", func(t *testing.T) {
		containers.SeedAuditIndex(t, admin, 598, "helpdesk")
		reader := auditClient(t, vk, "audit", containers.ValkeyACLEvents(), containers.ValkeyACLAuditReader())
		apps, err := streamsvalkey.AuditAppsWith(t.Context(), reader, streamsvalkey.AuditHooks{MemberLimit: 600})
		if err != nil || !slices.Equal(apps, []string{"helpdesk"}) {
			t.Fatalf("AuditApps = %v, err = %v", apps, err)
		}
	})

	// I7h: no second check runs after an SREM error.
	t.Run("I7h_no_second_check_after_srem_error", func(t *testing.T) {
		containers.SeedAuditIndex(t, admin, 1, "helpdesk")
		reader := auditOldReader(t, vk, admin)
		var checks atomic.Int32
		apps, err := streamsvalkey.AuditAppsWith(t.Context(), reader, streamsvalkey.AuditHooks{
			SecondCheck: func() { checks.Add(1) },
		})
		if err != nil || !slices.Equal(apps, []string{"helpdesk"}) {
			t.Fatalf("AuditApps = %v, err = %v", apps, err)
		}
		if n := checks.Load(); n != 0 {
			t.Errorf("second checks = %d, want 0", n)
		}
		if got := containers.AuditIndexMembers(t, admin); len(got) != 2 {
			t.Errorf("set = %v, want the forged member to stay", got)
		}
	})

	// I7i: the set has exactly as many members as the limit. SSCAN can return a
	// cursor that is not zero and then an empty page. This is not a truncation.
	t.Run("I7i_set_at_limit", func(t *testing.T) {
		containers.SeedAuditIndex(t, admin, 499, "helpdesk")
		reader := auditClient(t, vk, "audit", containers.ValkeyACLEvents(), containers.ValkeyACLAuditReader())
		apps, err := streamsvalkey.AuditAppsWith(t.Context(), reader, streamsvalkey.AuditHooks{MemberLimit: 500})
		if err != nil || !slices.Equal(apps, []string{"helpdesk"}) {
			t.Fatalf("AuditApps = %v, err = %v", apps, err)
		}
		if got := containers.AuditIndexMembers(t, admin); !slices.Equal(got, []string{"helpdesk"}) {
			t.Errorf("set has %d members, want [helpdesk]", len(got))
		}
	})

	// I7j: a reader without +srem reads a limited number of members in each call,
	// and the call returns ErrTooManyAuditApps. The members stay in the set.
	t.Run("I7j_no_srem_bounds_the_scan", func(t *testing.T) {
		const noSremLimit = 1000
		containers.SeedAuditIndex(t, admin, 3000, "helpdesk")
		reader := auditOldReader(t, vk, admin)
		for call := 1; call <= 2; call++ {
			before := xinfoStreamCalls(t, admin)
			_, err := streamsvalkey.AuditAppsWith(t.Context(), reader, streamsvalkey.AuditHooks{NoSremLimit: noSremLimit})
			if !errors.Is(err, streamsvalkey.ErrTooManyAuditApps) {
				t.Fatalf("call %d: err = %v, want ErrTooManyAuditApps", call, err)
			}
			if n := xinfoStreamCalls(t, admin) - before; n > noSremLimit {
				t.Errorf("call %d: %d XINFO STREAM commands, want a maximum of %d", call, n, noSremLimit)
			}
		}
		if got := containers.AuditIndexMembers(t, admin); len(got) != 3001 {
			t.Errorf("set has %d members, want 3001", len(got))
		}
	})

	// I7k: an SREM error that is not NOPERM stops the call.
	t.Run("I7k_srem_error_stops_the_call", func(t *testing.T) {
		containers.SeedAuditIndex(t, admin, 1, "helpdesk")
		reader := auditClient(t, vk, "audit", containers.ValkeyACLEvents(), containers.ValkeyACLAuditReader())
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		apps, err := streamsvalkey.AuditAppsWith(ctx, reader, streamsvalkey.AuditHooks{BeforeSrem: cancel})
		if !errors.Is(err, context.Canceled) || apps != nil {
			t.Fatalf("AuditApps = %v, err = %v, want a context.Canceled error", apps, err)
		}
	})
}
