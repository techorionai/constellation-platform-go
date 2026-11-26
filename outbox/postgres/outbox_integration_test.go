//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	valkeygo "github.com/valkey-io/valkey-go"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"golang.org/x/sync/errgroup"

	"github.com/techorionai/constellation-platform-go/auth"
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

func migrateOutbox(ctx context.Context, pool *pgxpool.Pool) (migrate.Result, error) {
	migrator, err := pgmigrate.NewMigrator(pool, outboxpg.Migrations(),
		pgmigrate.MigratorOptions{MigrationsTable: outboxpg.MigrationsTable})
	if err != nil {
		return migrate.Result{}, err
	}
	return migrate.Run(ctx, pgmigrate.NewLocker(pool, outboxpg.MigrationsTable), migrator,
		migrate.Options{Name: outboxpg.MigrationsTable})
}

type fixture struct {
	pool   *pgxpool.Pool
	tx     *txpg.Transactor
	writer *outbox.EventWriter
	store  *outboxpg.Store
}

func newFixture(t *testing.T, pg *containers.PostgresInstance, vk *containers.ValkeyInstance) fixture {
	t.Helper()
	admin := vk.Admin(t)
	if err := admin.Do(t.Context(), admin.B().Del().Key(streams.StreamKey(app)).Build()).Error(); err != nil {
		t.Fatal(err)
	}
	t.Helper()
	dbURL, name := pg.NewDatabase(t)
	c, err := clientpg.New(t.Context(), clientpg.Config{URL: dbURL, Database: name, MaxConns: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if _, err := migrateOutbox(t.Context(), c.Pool); err != nil {
		t.Fatal(err)
	}
	w, err := outboxpg.NewWriter(c.Pool, outbox.Config{App: app, Types: []string{eventType}})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{pool: c.Pool, tx: txpg.NewTransactor(c.Pool), writer: w, store: outboxpg.NewStore(c.Pool)}
}

func (f fixture) write(ctx context.Context, n int, tag string) error {
	return f.tx.WithinTx(ctx, func(ctx context.Context) error {
		events := make([]outbox.Event, n)
		for i := range events {
			events[i] = outbox.Event{
				Type: eventType, Subject: fmt.Sprintf("%s-%d", tag, i),
				Data:  map[string]any{"n": i, "tag": tag},
				Actor: outbox.ServiceActor("helpdesk-svc"),
			}
		}
		return f.writer.Write(ctx, events...)
	})
}

func (f fixture) count(t *testing.T, where string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM platform_outbox WHERE "+where).Scan(&n); err != nil {
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

func streamEventIDs(t *testing.T, c valkeygo.Client) map[string]int {
	t.Helper()
	entries, err := c.Do(t.Context(), c.B().Xrange().Key(streams.StreamKey(app)).Start("-").End("+").Build()).AsXRange()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int{}
	for _, e := range entries {
		var ce struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(e.FieldValues["event"]), &ce); err != nil {
			t.Fatal(err)
		}
		ids[ce.ID]++
	}
	return ids
}

func runRelay(t *testing.T, r *outbox.Relay) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func TestIntegration(t *testing.T) {
	pg := containers.Postgres(t)
	vk := containers.Valkey(t)

	// I1: a write in a transaction that rolls back leaves no row.
	t.Run("I1_rollback", func(t *testing.T) {
		f := newFixture(t, pg, vk)
		boom := errors.New("boom")
		err := f.tx.WithinTx(t.Context(), func(ctx context.Context) error {
			if err := f.writer.Write(ctx, outbox.Event{Type: eventType, Data: map[string]any{}, Actor: outbox.ServiceActor("svc")}); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if n := f.count(t, "true"); n != 0 {
			t.Fatalf("rows = %d, want 0", n)
		}
		// A write with no transaction is refused.
		err = f.writer.Write(t.Context(), outbox.Event{Type: eventType, Data: map[string]any{}, Actor: outbox.ServiceActor("svc")})
		if !errors.Is(err, outbox.ErrNoTransaction) {
			t.Fatalf("err = %v, want ErrNoTransaction", err)
		}
	})

	// I2: a write in a transaction that commits gives one row.
	t.Run("I2_commit", func(t *testing.T) {
		f := newFixture(t, pg, vk)
		if err := f.write(t.Context(), 1, "a"); err != nil {
			t.Fatal(err)
		}
		var payload, typ string
		var published *time.Time
		err := f.pool.QueryRow(t.Context(), "SELECT payload::text, type, published_at FROM platform_outbox").Scan(&payload, &typ, &published)
		if err != nil {
			t.Fatal(err)
		}
		if published != nil || typ != eventType {
			t.Errorf("published = %v, type = %q", published, typ)
		}
		for _, want := range []string{`"specversion": "1.0"`, `"source": "/helpdesk"`, `"actor": "helpdesk-svc"`, `"actortype": "service"`, `"subject": "a-0"`} {
			if !strings.Contains(payload, want) {
				t.Errorf("payload lacks %s: %s", want, payload)
			}
		}
	})

	// I3: the migrations run two times and with two instances at the same time.
	t.Run("I3_migrations", func(t *testing.T) {
		dbURL, name := pg.NewDatabase(t)
		c, err := clientpg.New(t.Context(), clientpg.Config{URL: dbURL, Database: name, MaxConns: 8})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		var g errgroup.Group
		for range 2 {
			g.Go(func() error { _, err := migrateOutbox(t.Context(), c.Pool); return err })
		}
		if err := g.Wait(); err != nil {
			t.Fatal(err)
		}
		if _, err := migrateOutbox(t.Context(), c.Pool); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := c.Pool.QueryRow(t.Context(), "SELECT count(*) FROM "+outboxpg.MigrationsTable).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("migration rows = %d, want 1", n)
		}
	})

	// I4: two relays on one database publish each event at least one time.
	t.Run("I4_two_relays", func(t *testing.T) {
		f := newFixture(t, pg, vk)
		admin := vk.Admin(t)
		pub := streamsvalkey.NewPublisher(admin, app)
		for i := range 50 {
			if err := f.write(t.Context(), 10, fmt.Sprintf("tx%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		for range 2 {
			r, err := outbox.NewRelay(outboxpg.NewStore(f.pool), pub, outbox.RelayOptions{BatchSize: 40})
			if err != nil {
				t.Fatal(err)
			}
			runRelay(t, r)
		}
		waitFor(t, "all rows published", 30*time.Second, func() bool { return f.count(t, "published_at IS NULL") == 0 })
		ids := streamEventIDs(t, admin)
		if len(ids) != 500 {
			t.Fatalf("distinct events in the stream = %d, want 500", len(ids))
		}
		var published int
		if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM platform_outbox WHERE published_at IS NOT NULL").Scan(&published); err != nil || published != 500 {
			t.Fatalf("published rows = %d, %v", published, err)
		}
	})

	// I5: a transaction that commits after a transaction with a higher ID.
	t.Run("I5_late_commit", func(t *testing.T) {
		f := newFixture(t, pg, vk)
		admin := vk.Admin(t)
		r, err := outbox.NewRelay(f.store, streamsvalkey.NewPublisher(admin, app), outbox.RelayOptions{Interval: 100 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		runRelay(t, r)

		started, commit := make(chan struct{}), make(chan struct{})
		var once sync.Once
		release := func() { once.Do(func() { close(commit) }) }
		t.Cleanup(release)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := f.tx.WithinTx(t.Context(), func(ctx context.Context) error {
				if err := f.writer.Write(ctx, outbox.Event{Type: eventType, Subject: "late", Data: map[string]any{}, Actor: outbox.ServiceActor("svc")}); err != nil {
					return err
				}
				close(started)
				<-commit
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
		<-started
		if err := f.write(t.Context(), 1, "early"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the higher ID is published", 20*time.Second, func() bool { return len(streamEventIDs(t, admin)) == 1 })
		release()
		wg.Wait()
		waitFor(t, "both events are published", 20*time.Second, func() bool { return len(streamEventIDs(t, admin)) == 2 })
	})

	// I6: a publish that fails keeps the rows, and the lag gauge goes up.
	t.Run("I6_publish_fails", func(t *testing.T) {
		f := newFixture(t, pg, vk)
		admin := vk.Admin(t)
		reader := sdkmetric.NewManualReader()
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
		setMax := func(v string) {
			if err := admin.Do(t.Context(), admin.B().Arbitrary("CONFIG", "SET").Args("maxmemory", v).Build()).Error(); err != nil {
				t.Fatal(err)
			}
		}
		lag := func() float64 {
			var rm metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &rm); err != nil {
				t.Fatal(err)
			}
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					if m.Name == "constellation.outbox.lag" {
						if dps := m.Data.(metricdata.Gauge[float64]).DataPoints; len(dps) > 0 {
							return dps[0].Value
						}
					}
				}
			}
			return -1
		}
		// 1 byte is less than the memory in use, so XADD gets an OOM error.
		setMax("1")
		t.Cleanup(func() {
			_ = admin.Do(context.Background(), admin.B().Arbitrary("CONFIG", "SET").Args("maxmemory", "0").Build()).Error()
		})
		if err := f.write(t.Context(), 3, "oom"); err != nil {
			t.Fatal(err)
		}
		r, err := outbox.NewRelay(f.store, streamsvalkey.NewPublisher(admin, app), outbox.RelayOptions{MeterProvider: mp, Interval: 100 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		runRelay(t, r)
		time.Sleep(1500 * time.Millisecond)
		if n := f.count(t, "published_at IS NULL"); n != 3 {
			t.Fatalf("unpublished rows = %d, want 3", n)
		}
		if v := lag(); v <= 0 {
			t.Fatalf("lag = %v, want more than 0", v)
		}
		setMax("0")
		waitFor(t, "the rows are published after the reset", 30*time.Second, func() bool { return f.count(t, "published_at IS NULL") == 0 })
		if v := lag(); v != 0 {
			t.Fatalf("lag = %v, want 0", v)
		}
	})

	// I7: the table purger keeps the unpublished rows.
	t.Run("I7_retention", func(t *testing.T) {
		f := newFixture(t, pg, vk)
		if err := f.write(t.Context(), 4, "r"); err != nil {
			t.Fatal(err)
		}
		// Row 1: published long ago. Row 2: published now. Rows 3 and 4: not
		// published, one of them old.
		stmts := []string{
			"UPDATE platform_outbox SET published_at = now() - interval '4 days' WHERE id = (SELECT min(id) FROM platform_outbox)",
			"UPDATE platform_outbox SET published_at = now() WHERE id = (SELECT min(id) + 1 FROM platform_outbox)",
			"UPDATE platform_outbox SET created_at = now() - interval '5 days' WHERE id = (SELECT min(id) + 2 FROM platform_outbox)",
		}
		for _, s := range stmts {
			if _, err := f.pool.Exec(t.Context(), s); err != nil {
				t.Fatal(err)
			}
		}
		p, err := retentionpg.NewTablePurger(f.pool, retentionpg.TableSpec{Table: outboxpg.Table, TimeColumn: "published_at"})
		if err != nil {
			t.Fatal(err)
		}
		n, err := p.Purge(t.Context(), retention.PurgeParams{Before: time.Now().Add(-72 * time.Hour), Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 || f.count(t, "true") != 3 || f.count(t, "published_at IS NULL") != 2 {
			t.Fatalf("deleted = %d, rows = %d, unpublished = %d", n, f.count(t, "true"), f.count(t, "published_at IS NULL"))
		}
	})

	// I15: end to end with the ACL selectors.
	t.Run("I15_end_to_end", func(t *testing.T) {
		f := newFixture(t, pg, vk)
		producer := aclClient(t, vk, app)
		consumerClient := aclClient(t, vk, "billing")

		tp := sdktrace.NewTracerProvider()
		reqCtx, span := tp.Tracer("test").Start(t.Context(), "request")
		defer span.End()
		err := f.tx.WithinTx(reqCtx, func(ctx context.Context) error {
			return f.writer.Write(ctx, outbox.Event{
				Type: eventType, Subject: "T-9", Data: map[string]any{"id": "T-9"},
				Actor: outbox.ActorFromPrincipal(auth.Principal{ID: "user-1", Type: auth.PrincipalUser, Act: &auth.Actor{ID: "agent-1", Type: auth.PrincipalAgent}}),
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		r, err := outbox.NewRelay(f.store, streamsvalkey.NewPublisher(producer, app), outbox.RelayOptions{Interval: 100 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		runRelay(t, r)

		group, err := streamsvalkey.NewGroup(consumerClient, streamsvalkey.GroupConfig{App: "billing", Stream: app, Group: "billing", StartID: "0"})
		if err != nil {
			t.Fatal(err)
		}
		spans := tracetest.NewSpanRecorder()
		got := make(chan streams.Message, 1)
		c, err := streams.NewConsumer(group, func(_ context.Context, m streams.Message) error {
			select {
			case got <- m:
			default:
			}
			return nil
		}, streams.ConsumerOptions{Block: 200 * time.Millisecond, TracerProvider: sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = c.Run(ctx) }()
		defer func() { cancel(); <-done }()
		select {
		case m := <-got:
			var ce map[string]any
			if err := json.Unmarshal(m.Event, &ce); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"type": eventType, "actor": "user-1", "actortype": "user", "act": "agent-1", "acttype": "agent", "source": "/helpdesk", "subject": "T-9"}
			for k, v := range want {
				if ce[k] != v {
					t.Errorf("%s = %v, want %v", k, ce[k], v)
				}
			}
			if _, err := uuid.Parse(fmt.Sprint(ce["id"])); err != nil {
				t.Errorf("id = %v", ce["id"])
			}
			waitFor(t, "the span ends", 5*time.Second, func() bool { return len(spans.Ended()) > 0 })
			links := spans.Ended()[0].Links()
			if len(links) != 1 || links[0].SpanContext.TraceID() != span.SpanContext().TraceID() {
				t.Errorf("links = %v, want a link to trace %s", links, span.SpanContext().TraceID())
			}
		case <-time.After(20 * time.Second):
			t.Fatal("the consumer got no event")
		}
	})
}
