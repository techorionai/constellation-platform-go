//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/techorionai/constellation-platform-go/clients/postgres"
	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/resources/test/containers"
)

// withReader sets the global meter provider to one with an in-memory reader,
// and restores the old provider at cleanup.
func withReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	old := otel.GetMeterProvider()
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(old) })
	return reader
}

type point struct {
	attrs attribute.Set
	value float64
}

// collect returns the data points of each metric by name.
func collect(t *testing.T, r *sdkmetric.ManualReader) map[string][]point {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string][]point{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					out[m.Name] = append(out[m.Name], point{dp.Attributes, float64(dp.Value)})
				}
			case metricdata.Sum[float64]:
				for _, dp := range d.DataPoints {
					out[m.Name] = append(out[m.Name], point{dp.Attributes, dp.Value})
				}
			}
		}
	}
	return out
}

// value returns the value of the data point that has all the attributes.
func value(t *testing.T, m map[string][]point, name string, kv ...attribute.KeyValue) float64 {
	t.Helper()
	for _, p := range m[name] {
		ok := true
		for _, a := range kv {
			if v, found := p.attrs.Value(a.Key); !found || v != a.Value {
				ok = false
			}
		}
		if ok {
			return p.value
		}
	}
	t.Fatalf("metric %s with %v not found in %v", name, kv, m[name])
	return 0
}

func configFor(url config.Secret, database string) postgres.Config {
	return postgres.Config{URL: url, Database: database}
}

// I1
func TestIntegrationConnect(t *testing.T) {
	pg := containers.Postgres(t)
	dbURL, name := pg.NewDatabase(t)
	c, err := postgres.New(t.Context(), configFor(dbURL, name))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var got string
	if err := c.Pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != name {
		t.Fatalf("got %q, want %q", got, name)
	}
}

// I3
func TestIntegrationWrongPassword(t *testing.T) {
	pg := containers.Postgres(t)
	dbURL, name := pg.NewDatabase(t)
	u, _ := url.Parse(dbURL.Reveal())
	u.User = url.UserPassword(u.User.Username(), "wr0ng-pass")
	_, err := postgres.New(t.Context(), configFor(config.Secret(u.String()), name))
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "wr0ng-pass") {
		t.Fatalf("error has the password: %v", err)
	}
}

// I7
func TestIntegrationMissingDatabase(t *testing.T) {
	pg := containers.Postgres(t)
	c, err := postgres.New(t.Context(), configFor(pg.AdminURL(), "missing_db"))
	if err == nil || c != nil {
		t.Fatalf("got %v %v", c, err)
	}
	conn, err := pgx.Connect(t.Context(), pg.AdminURL().Reveal())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context()) //nolint:errcheck
	var n int
	if err := conn.QueryRow(t.Context(),
		"SELECT count(*) FROM pg_stat_activity WHERE datname = 'missing_db'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d connections remain", n)
	}
}

// I8
func TestIntegrationDatabaseUserScope(t *testing.T) {
	pg := containers.Postgres(t)
	_, dbA := pg.NewDatabase(t)
	_, dbB := pg.NewDatabase(t)
	pg.Exec(t, "CREATE ROLE svc_a LOGIN PASSWORD 'svc-a-pass'")
	pg.Exec(t, "REVOKE CONNECT ON DATABASE "+dbA+" FROM PUBLIC")
	pg.Exec(t, "REVOKE CONNECT ON DATABASE "+dbB+" FROM PUBLIC")
	pg.Exec(t, "GRANT CONNECT ON DATABASE "+dbA+" TO svc_a")
	roleURL := config.Secret("postgres://svc_a:svc-a-pass@" + pg.Host() + "?sslmode=disable")

	c, err := postgres.New(t.Context(), configFor(roleURL, dbA))
	if err != nil {
		t.Fatalf("own database: %v", err)
	}
	c.Close()

	_, err = postgres.New(t.Context(), configFor(roleURL, dbB))
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("other database: %v", err)
	}
}

// I10
func TestIntegrationPoolMetrics(t *testing.T) {
	reader := withReader(t)
	pg := containers.Postgres(t)
	dbURL, name := pg.NewDatabase(t)
	cfg := configFor(dbURL, name)
	cfg.MaxConns, cfg.MinConns = 5, 2
	c, err := postgres.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for c.Pool.Stat().IdleConns() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("pool did not make 2 idle connections")
		}
		time.Sleep(20 * time.Millisecond)
	}
	pool := attribute.String("db.client.connection.pool.name", name)
	idle := attribute.String("pgxpool.acquire.result", "idle")
	first := collect(t, reader)
	idleBefore := value(t, first, "pgxpool.acquires", pool, idle)

	var held []*pgxpool.Conn
	for range 2 {
		conn, err := c.Pool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
	}
	second := collect(t, reader)
	for _, h := range held {
		h.Release()
	}

	if got := value(t, second, "db.client.connection.count", pool, attribute.String("db.client.connection.state", "used")); got != 2 {
		t.Errorf("used = %v", got)
	}
	if got := value(t, second, "db.client.connection.max", pool); got != 5 {
		t.Errorf("max = %v", got)
	}
	if got := value(t, second, "db.client.connection.idle.min", pool); got != 2 {
		t.Errorf("idle.min = %v", got)
	}
	if got := value(t, second, "pgxpool.acquires", pool, idle); got != idleBefore+2 {
		t.Errorf("idle acquires = %v, want %v", got, idleBefore+2)
	}
	if got := value(t, second, "pgxpool.connections.created", pool); got < 2 {
		t.Errorf("created = %v", got)
	}
	if got := value(t, second, "pgxpool.connection.constructing", pool); got < 0 {
		t.Errorf("constructing = %v", got)
	}
	for _, reason := range []string{"max_lifetime", "max_idle"} {
		if got := value(t, second, "pgxpool.connections.closed", pool, attribute.String("pgxpool.close.reason", reason)); got != 0 {
			t.Errorf("closed %s = %v", reason, got)
		}
	}
}

// I11
func TestIntegrationAcquireMetrics(t *testing.T) {
	reader := withReader(t)
	pg := containers.Postgres(t)
	dbURL, name := pg.NewDatabase(t)
	cfg := configFor(dbURL, name)
	cfg.MaxConns = 1
	c, err := postgres.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	conn, err := c.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	_, err = c.Pool.Acquire(short)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline error, got %v", err)
	}
	done := make(chan error, 1)
	go func() {
		c2, err := c.Pool.Acquire(t.Context())
		if err == nil {
			c2.Release()
		}
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	conn.Release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	m := collect(t, reader)
	pool := attribute.String("db.client.connection.pool.name", name)
	result := func(v string) attribute.KeyValue { return attribute.String("pgxpool.acquire.result", v) }
	if got := value(t, m, "pgxpool.acquires", pool, result("canceled")); got != 1 {
		t.Errorf("canceled = %v", got)
	}
	if got := value(t, m, "pgxpool.acquires", pool, result("empty")); got < 1 {
		t.Errorf("empty = %v", got)
	}
	if got := value(t, m, "pgxpool.acquire.time", pool); got <= 0 {
		t.Errorf("acquire.time = %v", got)
	}
}

// I12
func TestIntegrationCloseRemovesMetrics(t *testing.T) {
	reader := withReader(t)
	pg := containers.Postgres(t)
	dbURL, name := pg.NewDatabase(t)
	c, err := postgres.New(t.Context(), configFor(dbURL, name))
	if err != nil {
		t.Fatal(err)
	}
	if len(collect(t, reader)) == 0 {
		t.Fatal("no metrics before Close")
	}
	c.Close()
	for metric, points := range collect(t, reader) {
		if strings.HasPrefix(metric, "db.client.connection.") || strings.HasPrefix(metric, "pgxpool.") {
			t.Errorf("metric %s has %d points after Close", metric, len(points))
		}
	}
}
