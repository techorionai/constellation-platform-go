//go:build integration

package postgres_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	valkeygo "github.com/valkey-io/valkey-go"

	"github.com/techorionai/constellation-platform-go/audit"
	auditpg "github.com/techorionai/constellation-platform-go/audit/postgres"
	clientpg "github.com/techorionai/constellation-platform-go/clients/postgres"
	"github.com/techorionai/constellation-platform-go/migrate"
	pgmigrate "github.com/techorionai/constellation-platform-go/migrate/postgres"
	"github.com/techorionai/constellation-platform-go/outbox"
	outboxpg "github.com/techorionai/constellation-platform-go/outbox/postgres"
	"github.com/techorionai/constellation-platform-go/resources/test/containers"
	"github.com/techorionai/constellation-platform-go/retention"
	retentionpg "github.com/techorionai/constellation-platform-go/retention/postgres"
	"github.com/techorionai/constellation-platform-go/streams"
	streamsvalkey "github.com/techorionai/constellation-platform-go/streams/valkey"
	txpg "github.com/techorionai/constellation-platform-go/transaction/postgres"
)

const (
	app       = "helpdesk"
	eventType = "helpdesk.ticket.created"
)

func runMigrations(ctx context.Context, pool *pgxpool.Pool) (outboxRes, auditRes migrate.Result, err error) {
	outboxM, err := pgmigrate.NewMigrator(pool, outboxpg.Migrations(),
		pgmigrate.MigratorOptions{MigrationsTable: outboxpg.MigrationsTable})
	if err != nil {
		return outboxRes, auditRes, err
	}
	outboxRes, err = migrate.Run(ctx, pgmigrate.NewLocker(pool, outboxpg.MigrationsTable), outboxM,
		migrate.Options{Name: outboxpg.MigrationsTable})
	if err != nil {
		return outboxRes, auditRes, err
	}
	auditM, err := pgmigrate.NewMigrator(pool, auditpg.Migrations(),
		pgmigrate.MigratorOptions{MigrationsTable: auditpg.MigrationsTable})
	if err != nil {
		return outboxRes, auditRes, err
	}
	auditRes, err = migrate.Run(ctx, pgmigrate.NewLocker(pool, auditpg.MigrationsTable), auditM,
		migrate.Options{Name: auditpg.MigrationsTable})
	return outboxRes, auditRes, err
}

type fixture struct {
	pool       *pgxpool.Pool
	tx         *txpg.Transactor
	events     *outbox.EventWriter
	audits     *audit.EntryWriter
	eventStore *outboxpg.Store
	auditStore *outboxpg.Store
}

func newFixture(t *testing.T, pg *containers.PostgresInstance, vk *containers.ValkeyInstance) fixture {
	t.Helper()
	admin := vk.Admin(t)
	err := admin.Do(t.Context(), admin.B().Del().Key(streams.KeyFor(streams.Events, app), streams.KeyFor(streams.Audit, app), streams.AuditAppsKey, streams.AuditIndexKey).Build()).Error() //nolint:staticcheck // the test uses the old set
	if err != nil {
		t.Fatal(err)
	}
	dbURL, name := pg.NewDatabase(t)
	c, err := clientpg.New(t.Context(), clientpg.Config{URL: dbURL, Database: name, MaxConns: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if _, _, err := runMigrations(t.Context(), c.Pool); err != nil {
		t.Fatal(err)
	}
	ew, err := outboxpg.NewWriter(c.Pool, outbox.Config{App: app, Types: []string{eventType}})
	if err != nil {
		t.Fatal(err)
	}
	aw, err := auditpg.NewWriter(c.Pool, audit.Config{App: app, ClientID: "helpdesk-svc"})
	if err != nil {
		t.Fatal(err)
	}
	as, err := outboxpg.NewStoreFor(c.Pool, auditpg.Table)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{
		pool: c.Pool, tx: txpg.NewTransactor(c.Pool), events: ew, audits: aw,
		eventStore: outboxpg.NewStore(c.Pool), auditStore: as,
	}
}

func (f fixture) count(t *testing.T, table, where string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table+" WHERE "+where).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func runRelay(t *testing.T, r *outbox.Relay) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

// newClient returns a client for an ACL user and the name of the user.
func newClient(t *testing.T, vk *containers.ValkeyInstance, prefix string, opts ...containers.ACLOption) (valkeygo.Client, string) {
	t.Helper()
	user, password := vk.NewACLUser(t, prefix, opts...)
	c, err := valkeygo.NewClient(valkeygo.ClientOption{
		InitAddress: []string{vk.Address()}, Username: user, Password: password.Reveal(), DisableCache: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, user
}

func do(c valkeygo.Client, args ...string) error {
	return c.Do(context.Background(), c.B().Arbitrary(args[0]).Args(args[1:]...).Build()).Error()
}

func isNoPerm(err error) bool { return err != nil && strings.Contains(err.Error(), "NOPERM") }

func streamIDs(t *testing.T, c valkeygo.Client, key string) []string {
	t.Helper()
	entries, err := c.Do(t.Context(), c.B().Xrange().Key(key).Start("-").End("+").Build()).AsXRange()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range entries {
		var ce struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(e.FieldValues["event"]), &ce); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ce.ID)
	}
	return ids
}

func (f fixture) writeAudit(ctx context.Context, summary string) error {
	return f.tx.WithinTx(ctx, func(ctx context.Context) error {
		return f.audits.Write(ctx, audit.Entry{
			Action: "helpdesk:ticket:write", Result: audit.Success,
			Target: &audit.Target{Type: "ticket", ID: "T-1"}, Summary: summary,
		})
	})
}

func TestIntegration(t *testing.T) {
	pg := containers.Postgres(t)
	vk := containers.Valkey(t)

	// I1: the migrations of both tables run two times.
	t.Run("I1_migrations", func(t *testing.T) {
		dbURL, name := pg.NewDatabase(t)
		c, err := clientpg.New(t.Context(), clientpg.Config{URL: dbURL, Database: name, MaxConns: 8})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		o1, a1, err := runMigrations(t.Context(), c.Pool)
		if err != nil {
			t.Fatal(err)
		}
		if o1.Applied == 0 || a1.Applied == 0 {
			t.Fatalf("first run applied outbox %v, audit %v", o1.Applied, a1.Applied)
		}
		for _, table := range []string{outboxpg.Table, auditpg.Table} {
			var ok bool
			if err := c.Pool.QueryRow(t.Context(), "SELECT to_regclass($1) IS NOT NULL", table).Scan(&ok); err != nil || !ok {
				t.Errorf("table %s: ok = %v, err = %v", table, ok, err)
			}
		}
		o2, a2, err := runMigrations(t.Context(), c.Pool)
		if err != nil {
			t.Fatal(err)
		}
		if o2.Applied != 0 || a2.Applied != 0 {
			t.Errorf("second run applied outbox %v, audit %v", o2.Applied, a2.Applied)
		}
	})

	// I2: a rollback leaves no audit row. A commit leaves one row.
	t.Run("I2_transaction", func(t *testing.T) {
		f := newFixture(t, pg, vk)
		boom := errors.New("boom")
		err := f.tx.WithinTx(t.Context(), func(ctx context.Context) error {
			if err := f.audits.Write(ctx, audit.Entry{Action: "helpdesk:ticket:write", Result: audit.Success}); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if n := f.count(t, auditpg.Table, "true"); n != 0 {
			t.Fatalf("rows after rollback = %d", n)
		}
		if err := f.audits.Write(t.Context(), audit.Entry{Action: "helpdesk:ticket:write", Result: audit.Success}); !errors.Is(err, audit.ErrNoTransaction) {
			t.Fatalf("write with no transaction: err = %v", err)
		}
		if err := f.writeAudit(t.Context(), "created"); err != nil {
			t.Fatal(err)
		}
		var payload, typ string
		if err := f.pool.QueryRow(t.Context(), "SELECT payload::text, type FROM platform_audit_outbox").Scan(&payload, &typ); err != nil {
			t.Fatal(err)
		}
		if typ != "helpdesk.audit.recorded" {
			t.Errorf("type = %q", typ)
		}
		for _, want := range []string{`"source": "/helpdesk"`, `"actor": "helpdesk-svc"`, `"actortype": "service"`, `"action": "helpdesk:ticket:write"`, `"result": "success"`, `"summary": "created"`} {
			if !strings.Contains(payload, want) {
				t.Errorf("payload lacks %s: %s", want, payload)
			}
		}
		// The event outbox is not changed.
		if n := f.count(t, outboxpg.Table, "true"); n != 0 {
			t.Errorf("event outbox rows = %d", n)
		}
	})

	// I3: the audit relay and the event relay on one database.
	t.Run("I3_two_relays", func(t *testing.T) {
		f := newFixture(t, pg, vk)
		admin := vk.Admin(t)
		producer, _ := newClient(t, vk, app, containers.ValkeyACLEvents(), containers.ValkeyACLAuditProducer())

		evRelay, err := outbox.NewRelay(f.eventStore, streamsvalkey.NewPublisher(producer, app), outbox.RelayOptions{Interval: 100 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		auRelay, err := outbox.NewRelay(f.auditStore,
			streamsvalkey.NewPublisher(producer, app, streamsvalkey.WithFamily(streams.Audit)),
			outbox.RelayOptions{Name: "audit", Interval: 100 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		runRelay(t, evRelay)
		runRelay(t, auRelay)

		err = f.tx.WithinTx(t.Context(), func(ctx context.Context) error {
			if err := f.events.Write(ctx, outbox.Event{Type: eventType, Data: map[string]any{"id": 1}, Actor: outbox.ServiceActor("svc")}); err != nil {
				return err
			}
			return f.audits.Write(ctx, audit.Entry{Action: "helpdesk:ticket:write", Result: audit.Success})
		})
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, "both streams", 10*time.Second, func() bool {
			return len(streamIDs(t, admin, streams.KeyFor(streams.Events, app))) == 1 &&
				len(streamIDs(t, admin, streams.KeyFor(streams.Audit, app))) == 1
		})
		var evType, auType string
		if err := f.pool.QueryRow(t.Context(), "SELECT type FROM platform_outbox").Scan(&evType); err != nil {
			t.Fatal(err)
		}
		if err := f.pool.QueryRow(t.Context(), "SELECT type FROM platform_audit_outbox").Scan(&auType); err != nil {
			t.Fatal(err)
		}
		if evType != eventType || auType != "helpdesk.audit.recorded" {
			t.Errorf("types = %q, %q", evType, auType)
		}
		for _, key := range []string{streams.AuditAppsKey, streams.AuditIndexKey} { //nolint:staticcheck // the test checks the old set
			n, err := admin.Do(t.Context(), admin.B().Exists().Key(key).Build()).AsInt64()
			if err != nil || n != 0 {
				t.Errorf("EXISTS %s = %d, err = %v, want 0", key, n, err)
			}
		}
	})

	// I4: the audit consumer, the family functions, and the lag.
	t.Run("I4_consumer", func(t *testing.T) {
		f := newFixture(t, pg, vk)
		admin := vk.Admin(t)
		producer, _ := newClient(t, vk, app, containers.ValkeyACLEvents(), containers.ValkeyACLAuditProducer())
		reader, _ := newClient(t, vk, "audit", containers.ValkeyACLEvents(), containers.ValkeyACLAuditReader())

		// An event on the event stream, so the family functions can be told apart.
		if err := streamsvalkey.NewPublisher(producer, app).Publish(t.Context(), [][]byte{[]byte(`{"id":"e"}`)}); err != nil {
			t.Fatal(err)
		}
		g, err := streamsvalkey.NewGroup(reader, streamsvalkey.GroupConfig{
			App: "audit", Stream: app, Family: streams.Audit, Group: "audit", StartID: "0",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := g.Ensure(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := f.writeAudit(t.Context(), "one"); err != nil {
			t.Fatal(err)
		}
		relay, err := outbox.NewRelay(f.auditStore,
			streamsvalkey.NewPublisher(producer, app, streamsvalkey.WithFamily(streams.Audit)),
			outbox.RelayOptions{Name: "audit", Interval: 100 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		runRelay(t, relay)
		waitFor(t, "the audit entry", 10*time.Second, func() bool {
			return len(streamIDs(t, admin, streams.KeyFor(streams.Audit, app))) == 1
		})

		lag, err := streamsvalkey.LagFor(t.Context(), reader, streams.Audit, app, "audit")
		if err != nil || lag <= 0 {
			t.Errorf("lag before = %v, err = %v, want more than 0", lag, err)
		}
		groups, err := streamsvalkey.GroupsFor(t.Context(), reader, streams.Audit, app)
		if err != nil || !slices.Equal(groups, []string{"audit"}) {
			t.Errorf("GroupsFor = %v, err = %v", groups, err)
		}
		if eg, err := streamsvalkey.Groups(t.Context(), reader, app); err != nil || len(eg) != 0 {
			t.Errorf("Groups (events) = %v, err = %v", eg, err)
		}
		if _, err := streamsvalkey.Lag(t.Context(), reader, app, "audit"); !errors.Is(err, streamsvalkey.ErrNoGroup) {
			t.Errorf("Lag (events) err = %v, want ErrNoGroup", err)
		}

		var mu sync.Mutex
		var got []string
		c, err := streams.NewConsumer(g, func(_ context.Context, m streams.Message) error {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, string(m.Event))
			return nil
		}, streams.ConsumerOptions{Block: 100 * time.Millisecond, NeverDrop: true})
		if err != nil {
			t.Fatal(err)
		}
		cctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); _ = c.Run(cctx) }()
		defer func() { cancel(); <-done }()
		waitFor(t, "the handler", 10*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 1 })
		mu.Lock()
		if !strings.Contains(got[0], `"type": "helpdesk.audit.recorded"`) {
			t.Errorf("handler event = %s", got[0])
		}
		mu.Unlock()
		waitFor(t, "lag 0 after the ack", 10*time.Second, func() bool {
			l, err := streamsvalkey.LagFor(t.Context(), reader, streams.Audit, app, "audit")
			return err == nil && l == 0
		})
	})

	// I5: the ACL of an audit producer.
	t.Run("I5_producer_acl", func(t *testing.T) {
		vk.Admin(t)
		c, _ := newClient(t, vk, app, containers.ValkeyACLEvents(), containers.ValkeyACLAuditProducer())
		checks := []struct {
			name   string
			args   []string
			noPerm bool
		}{
			{"XADD own", []string{"XADD", "audit.stream:helpdesk", "*", "event", "{}"}, false},
			{"XADD other", []string{"XADD", "audit.stream:crm", "*", "event", "{}"}, true},
			{"SADD audit.apps", []string{"SADD", "audit.apps", "helpdesk"}, true},
			{"SREM audit.apps", []string{"SREM", "audit.apps", "helpdesk"}, true},
			{"SSCAN audit.apps", []string{"SSCAN", "audit.apps", "0"}, true},
			{"SADD audit.streams", []string{"SADD", "audit.streams", "helpdesk"}, true},
			{"XREADGROUP", []string{"XREADGROUP", "GROUP", "g", "c", "STREAMS", "audit.stream:helpdesk", ">"}, true},
		}
		for _, ch := range checks {
			err := do(c, ch.args...)
			if ch.noPerm != isNoPerm(err) || (!ch.noPerm && err != nil) {
				t.Errorf("%s: err = %v, want noperm = %v", ch.name, err, ch.noPerm)
			}
		}
	})

	// I6: the ACL of the audit service.
	t.Run("I6_reader_acl", func(t *testing.T) {
		admin := vk.Admin(t)
		if err := do(admin, "DEL", "audit.stream:helpdesk"); err != nil {
			t.Fatal(err)
		}
		if err := do(admin, "XADD", "audit.stream:helpdesk", "*", "event", "{}"); err != nil {
			t.Fatal(err)
		}
		c, _ := newClient(t, vk, "audit", containers.ValkeyACLEvents(), containers.ValkeyACLAuditReader())
		if err := do(c, "XGROUP", "CREATE", "audit.stream:helpdesk", "audit", "0"); err != nil {
			t.Fatalf("XGROUP CREATE: %v", err)
		}
		checks := []struct {
			name   string
			args   []string
			noPerm bool
		}{
			{"XREADGROUP", []string{"XREADGROUP", "GROUP", "audit", "c", "STREAMS", "audit.stream:helpdesk", ">"}, false},
			{"XACK", []string{"XACK", "audit.stream:helpdesk", "audit", "0-1"}, false},
			{"SSCAN audit.apps", []string{"SSCAN", "audit.apps", "0"}, false},
			{"XADD", []string{"XADD", "audit.stream:helpdesk", "*", "event", "{}"}, true},
			{"SADD audit.apps", []string{"SADD", "audit.apps", "x"}, true},
			{"SREM audit.apps", []string{"SREM", "audit.apps", "x"}, true},
			{"SSCAN audit.streams", []string{"SSCAN", "audit.streams", "0"}, true},
		}
		for _, ch := range checks {
			err := do(c, ch.args...)
			if ch.noPerm != isNoPerm(err) || (!ch.noPerm && err != nil) {
				t.Errorf("%s: err = %v, want noperm = %v", ch.name, err, ch.noPerm)
			}
		}
	})

	// I7: AuditApps reads the registered apps. It deletes no member.
	t.Run("I7_audit_apps", func(t *testing.T) {
		admin := vk.Admin(t)
		if err := do(admin, "DEL", streams.AuditAppsKey); err != nil {
			t.Fatal(err)
		}
		for _, a := range []string{"helpdesk", "crm", "Bad-App"} {
			if err := do(admin, "XADD", "audit.stream:"+a, "*", "event", "{}"); err != nil {
				t.Fatal(err)
			}
		}
		if err := do(admin, "SADD", streams.AuditAppsKey, "helpdesk", "crm", "Bad-App", "a000"); err != nil {
			t.Fatal(err)
		}
		reader, _ := newClient(t, vk, "audit", containers.ValkeyACLEvents(), containers.ValkeyACLAuditReader())
		apps, err := streamsvalkey.AuditApps(t.Context(), reader)
		if err != nil || !slices.Equal(apps, []string{"crm", "helpdesk"}) {
			t.Fatalf("AuditApps = %v, err = %v", apps, err)
		}
		if got := containers.AuditIndexMembers(t, admin); len(got) != 4 {
			t.Errorf("set = %v, want the 4 members", got)
		}
	})

	// I7b: more than 1000 registered apps with a stream. No member is deleted.
	t.Run("I7b_too_many_apps", func(t *testing.T) {
		admin := vk.Admin(t)
		if err := do(admin, "DEL", streams.AuditAppsKey); err != nil {
			t.Fatal(err)
		}
		list := make([]string, 0, 1001)
		for i := range 1001 {
			a := fmt.Sprintf("app%04d", i)
			if err := do(admin, "XADD", "audit.stream:"+a, "*", "event", "{}"); err != nil {
				t.Fatal(err)
			}
			list = append(list, a)
		}
		if err := do(admin, append([]string{"SADD", streams.AuditAppsKey}, list...)...); err != nil {
			t.Fatal(err)
		}
		reader, _ := newClient(t, vk, "audit", containers.ValkeyACLEvents(), containers.ValkeyACLAuditReader())
		apps, err := streamsvalkey.AuditApps(t.Context(), reader)
		if !errors.Is(err, streamsvalkey.ErrTooManyAuditApps) || len(apps) != 1000 || !slices.IsSorted(apps) || apps[0] != "app0000" || apps[999] != "app0999" {
			t.Fatalf("AuditApps: len = %d, err = %v", len(apps), err)
		}
		if got := containers.AuditIndexMembers(t, admin); len(got) != 1001 {
			t.Errorf("set has %d members, want 1001", len(got))
		}
	})

	// I8: the retention purgers of the audit outbox and the audit stream.
	t.Run("I8_retention", func(t *testing.T) {
		f := newFixture(t, pg, vk)
		admin := vk.Admin(t)
		producer, _ := newClient(t, vk, app, containers.ValkeyACLEvents(), containers.ValkeyACLAuditProducer())
		for i := range 2 {
			if err := f.writeAudit(t.Context(), fmt.Sprintf("n%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		relay, err := outbox.NewRelay(f.auditStore,
			streamsvalkey.NewPublisher(producer, app, streamsvalkey.WithFamily(streams.Audit)),
			outbox.RelayOptions{Name: "audit", Interval: 100 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = relay.Run(ctx) }()
		waitFor(t, "the published rows", 10*time.Second, func() bool { return f.count(t, auditpg.Table, "published_at IS NOT NULL") == 2 })
		cancel()
		<-done
		// One unpublished row stays.
		if err := f.writeAudit(t.Context(), "unpublished"); err != nil {
			t.Fatal(err)
		}

		tp, err := retentionpg.NewTablePurger(f.pool, retentionpg.TableSpec{Table: auditpg.Table, TimeColumn: "published_at"})
		if err != nil {
			t.Fatal(err)
		}
		// The cut-off is after the publish time of both rows. Wait until the clock
		// is in a later millisecond, because stream IDs have a resolution of 1 ms.
		before := time.Now()
		waitFor(t, "the clock after the cut-off", 5*time.Second, func() bool { return time.Now().UnixMilli() > before.UnixMilli() })
		n, err := tp.Purge(t.Context(), retention.PurgeParams{Before: before, Limit: 100})
		if err != nil || n != 2 {
			t.Fatalf("table purge = %d, err = %v", n, err)
		}
		if f.count(t, auditpg.Table, "published_at IS NULL") != 1 {
			t.Error("the unpublished row did not stay")
		}
		sp := streamsvalkey.NewTrimPurger(producer, app, streamsvalkey.WithFamily(streams.Audit))
		n, err = sp.Purge(t.Context(), retention.PurgeParams{Before: before, Limit: 100})
		if err != nil || n != 2 {
			t.Fatalf("stream purge = %d, err = %v", n, err)
		}
		if got := streamIDs(t, admin, streams.KeyFor(streams.Audit, app)); len(got) != 0 {
			t.Errorf("stream entries = %v", got)
		}
	})

	// I9: Publish sends no index command, also with WithAuditIndexRefresh(0).
	t.Run("I9_no_index_command", func(t *testing.T) {
		admin := vk.Admin(t)
		if err := do(admin, "DEL", streams.AuditAppsKey, streams.AuditIndexKey, "audit.stream:helpdesk"); err != nil { //nolint:staticcheck // the test checks the old set
			t.Fatal(err)
		}
		c, _ := newClient(t, vk, app, containers.ValkeyACLEvents(), containers.ValkeyACLAuditProducer())
		var logs lockedBuffer
		pub := streamsvalkey.NewPublisher(c, app, streamsvalkey.WithFamily(streams.Audit),
			streamsvalkey.WithAuditIndexRefresh(0), //nolint:staticcheck // I9 tests the deprecated option
			streamsvalkey.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
		payload := [][]byte{[]byte(`{"id":"secret-payload"}`)}
		for range 2 {
			if err := pub.Publish(t.Context(), payload); err != nil {
				t.Fatal(err)
			}
		}
		if got := streamIDs(t, admin, "audit.stream:helpdesk"); len(got) != 2 {
			t.Errorf("stream entries = %v, want 2", got)
		}
		for _, key := range []string{streams.AuditAppsKey, streams.AuditIndexKey} { //nolint:staticcheck // the test checks the old set
			n, err := admin.Do(t.Context(), admin.B().Exists().Key(key).Build()).AsInt64()
			if err != nil || n != 0 {
				t.Errorf("EXISTS %s = %d, err = %v, want 0", key, n, err)
			}
		}
		if strings.Contains(logs.String(), `"level":"WARN"`) {
			t.Errorf("warn record: %s", logs.String())
		}
	})
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
