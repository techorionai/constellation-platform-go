//go:build integration

package postgres_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	clientpg "github.com/techorionai/constellation-platform-go/clients/postgres"
	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/migrate"
	pgmigrate "github.com/techorionai/constellation-platform-go/migrate/postgres"
	"github.com/techorionai/constellation-platform-go/resources/test/containers"
	"github.com/techorionai/constellation-platform-go/secrets"
	secretspg "github.com/techorionai/constellation-platform-go/secrets/postgres"
	txpg "github.com/techorionai/constellation-platform-go/transaction/postgres"
)

// testKey returns a base64 test key. It is not a real key.
func testKey(seed byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{seed}, 32))
}

func testRing(t *testing.T, id string, seed byte, prevID string, prevSeed byte) *secrets.KeyRing {
	t.Helper()
	cfg := secrets.Config{MasterKey: config.Secret(testKey(seed)), MasterKeyID: id}
	if prevID != "" {
		cfg.PreviousKeys = config.Secret(prevID + ":" + testKey(prevSeed))
	}
	ring, err := secrets.NewKeyRing(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

type fixture struct {
	pool *pgxpool.Pool
	conn *pgx.Conn
	repo *secretspg.Repository
}

// migrateSecrets runs the secrets migrations with their own migrations table.
func migrateSecrets(ctx context.Context, pool *pgxpool.Pool) (migrate.Result, error) {
	migrator, err := pgmigrate.NewMigrator(pool, secretspg.Migrations(),
		pgmigrate.MigratorOptions{MigrationsTable: secretspg.MigrationsTable})
	if err != nil {
		return migrate.Result{}, err
	}
	return migrate.Run(ctx, pgmigrate.NewLocker(pool, secretspg.MigrationsTable), migrator,
		migrate.Options{Name: secretspg.MigrationsTable})
}

func newFixture(t *testing.T, pg *containers.PostgresInstance) fixture {
	t.Helper()
	dbURL, name := pg.NewDatabase(t)
	c, err := clientpg.New(t.Context(), clientpg.Config{URL: dbURL, Database: name, MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if _, err := migrateSecrets(t.Context(), c.Pool); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), dbURL.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return fixture{pool: c.Pool, conn: conn, repo: secretspg.NewRepository(c.Pool)}
}

func TestIntegrationSecrets(t *testing.T) {
	pg := containers.Postgres(t)

	// I1
	t.Run("I1 migrations table", func(t *testing.T) {
		reader := sdkmetric.NewManualReader()
		old := otel.GetMeterProvider()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
		t.Cleanup(func() { otel.SetMeterProvider(old) })

		dbURL, name := pg.NewDatabase(t)
		c, err := clientpg.New(t.Context(), clientpg.Config{URL: dbURL, Database: name, MaxConns: 4})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)

		// A service migration set with the default table. Both sets use version 1.
		service := fstest.MapFS{"000001_items.up.sql": {Data: []byte("CREATE TABLE items (id int);")}}
		sm, err := pgmigrate.NewMigrator(c.Pool, service, pgmigrate.MigratorOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := migrate.Run(t.Context(), pgmigrate.NewLocker(c.Pool, ""), sm, migrate.Options{}); err != nil {
			t.Fatal(err)
		}
		if _, err := migrateSecrets(t.Context(), c.Pool); err != nil {
			t.Fatal(err)
		}

		var n int
		if err := c.Pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.tables WHERE table_name = 'platform_secrets'`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("platform_secrets: n %d, err %v", n, err)
		}
		for _, table := range []string{"schema_migrations", "platform_secrets_schema_migrations"} {
			var v int
			if err := c.Pool.QueryRow(t.Context(), `SELECT version FROM `+table).Scan(&v); err != nil || v != 1 {
				t.Fatalf("%s: version %d, err %v", table, v, err)
			}
		}

		var rm metricdata.ResourceMetrics
		if err := reader.Collect(t.Context(), &rm); err != nil {
			t.Fatal(err)
		}
		names := map[string]uint64{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				h, ok := m.Data.(metricdata.Histogram[float64])
				if m.Name != "migrate.duration" || !ok {
					continue
				}
				for _, dp := range h.DataPoints {
					v, _ := dp.Attributes.Value(attribute.Key("migrate.name"))
					names[v.AsString()] += dp.Count
				}
			}
		}
		if names["schema_migrations"] != 1 || names["platform_secrets_schema_migrations"] != 1 {
			t.Fatalf("migrate.duration records = %v", names)
		}
	})

	// I2, I3, I4, I5, I6
	t.Run("I2 to I6 put get delete", func(t *testing.T) {
		f := newFixture(t, pg)
		ctx := t.Context()
		svc := secrets.NewService(testRing(t, "k1", 1, "", 0), f.repo)
		plain := []byte("PLAINTEXT-MARKER-0123456789")

		// I2
		if err := svc.Put(ctx, "a", plain); err != nil {
			t.Fatal(err)
		}
		got, err := svc.Get(ctx, "a")
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("got %q, err %v", got, err)
		}

		// I3
		var (
			keyID                string
			dek, nonce, cipherTx []byte
		)
		err = f.conn.QueryRow(ctx, `SELECT key_id, wrapped_dek, nonce, ciphertext FROM platform_secrets WHERE name = 'a'`).
			Scan(&keyID, &dek, &nonce, &cipherTx)
		if err != nil {
			t.Fatal(err)
		}
		if keyID != "k1" {
			t.Fatalf("key_id = %q", keyID)
		}
		for _, col := range [][]byte{dek, nonce, cipherTx, []byte(keyID)} {
			if bytes.Contains(col, plain) {
				t.Fatal("a column contains the plaintext")
			}
		}
		var rowText string
		if err := f.conn.QueryRow(ctx, `SELECT platform_secrets::text FROM platform_secrets WHERE name = 'a'`).Scan(&rowText); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains([]byte(rowText), plain) {
			t.Fatal("the row text contains the plaintext")
		}

		// I4
		if err := svc.Put(ctx, "a", []byte("second")); err != nil {
			t.Fatal(err)
		}
		if got, err := svc.Get(ctx, "a"); err != nil || string(got) != "second" {
			t.Fatalf("got %q, err %v", got, err)
		}
		var later bool
		if err := f.conn.QueryRow(ctx, `SELECT updated_at > created_at FROM platform_secrets WHERE name = 'a'`).Scan(&later); err != nil || !later {
			t.Fatalf("updated_at later = %v, err %v", later, err)
		}

		// I5
		if err := svc.Put(ctx, "b", []byte("other")); err != nil {
			t.Fatal(err)
		}
		if _, err := f.conn.Exec(ctx, `UPDATE platform_secrets b SET ciphertext = a.ciphertext, nonce = a.nonce, wrapped_dek = a.wrapped_dek
			FROM platform_secrets a WHERE a.name = 'a' AND b.name = 'b'`); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Get(ctx, "b"); !errors.Is(err, secrets.ErrDecrypt) {
			t.Fatalf("err = %v, want ErrDecrypt", err)
		}

		// I6
		if err := svc.Delete(ctx, "a"); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Get(ctx, "a"); !errors.Is(err, secrets.ErrNotFound) {
			t.Fatalf("Get: err = %v", err)
		}
		if err := svc.Delete(ctx, "does-not-exist"); !errors.Is(err, secrets.ErrNotFound) {
			t.Fatalf("Delete: err = %v", err)
		}
	})

	// I13
	t.Run("I13 transaction", func(t *testing.T) {
		f := newFixture(t, pg)
		ctx := t.Context()
		svc := secrets.NewService(testRing(t, "k1", 1, "", 0), f.repo)
		tx := txpg.NewTransactor(f.pool)
		boom := errors.New("boom")
		err := tx.WithinTx(ctx, func(ctx context.Context) error {
			if err := svc.Put(ctx, "in-tx", []byte("value")); err != nil {
				return err
			}
			if _, err := svc.Get(ctx, "in-tx"); err != nil {
				return fmt.Errorf("the transaction does not see its own secret: %w", err)
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if _, err := svc.Get(ctx, "in-tx"); !errors.Is(err, secrets.ErrNotFound) {
			t.Fatalf("after rollback: err = %v, want ErrNotFound", err)
		}
		// A commit keeps the secret.
		if err := tx.WithinTx(ctx, func(ctx context.Context) error { return svc.Put(ctx, "in-tx", []byte("value")) }); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Get(ctx, "in-tx"); err != nil {
			t.Fatalf("after commit: %v", err)
		}
	})

	// I7
	t.Run("I7 rewrap 250", func(t *testing.T) {
		f := newFixture(t, pg)
		ctx := t.Context()
		ring1 := testRing(t, "k1", 1, "", 0)
		svc1 := secrets.NewService(ring1, f.repo)
		for i := 0; i < 250; i++ {
			if err := svc1.Put(ctx, fmt.Sprintf("s%03d", i), []byte(fmt.Sprintf("value-%d", i))); err != nil {
				t.Fatal(err)
			}
		}
		svc2 := secrets.NewService(testRing(t, "k2", 2, "k1", 1), f.repo)
		res, err := svc2.Rewrap(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		if res != (secrets.RewrapResult{Rewrapped: 250}) {
			t.Fatalf("result = %+v", res)
		}
		var other int
		if err := f.conn.QueryRow(ctx, `SELECT count(*) FROM platform_secrets WHERE key_id <> 'k2'`).Scan(&other); err != nil || other != 0 {
			t.Fatalf("rows with another key: %d, err %v", other, err)
		}
		only2 := secrets.NewService(testRing(t, "k2", 2, "", 0), f.repo)
		for i := 0; i < 250; i++ {
			got, err := only2.Get(ctx, fmt.Sprintf("s%03d", i))
			if err != nil || string(got) != fmt.Sprintf("value-%d", i) {
				t.Fatalf("s%03d: %q, %v", i, got, err)
			}
		}
		// A second Rewrap finds nothing to do.
		if res, err := svc2.Rewrap(ctx, 100); err != nil || res != (secrets.RewrapResult{}) {
			t.Fatalf("second result = %+v, err %v", res, err)
		}
	})

	t.Run("UpdateEnvelope conflict", func(t *testing.T) {
		f := newFixture(t, pg)
		ctx := t.Context()
		ring := testRing(t, "k1", 1, "", 0)
		svc := secrets.NewService(ring, f.repo)
		if err := svc.Put(ctx, "a", []byte("v")); err != nil {
			t.Fatal(err)
		}
		rec, err := f.repo.Get(ctx, "a")
		if err != nil {
			t.Fatal(err)
		}
		// A Put changes the wrapped key, so the old envelope no longer matches.
		if err := svc.Put(ctx, "a", []byte("w")); err != nil {
			t.Fatal(err)
		}
		newEnv, err := secrets.Rewrap(testRing(t, "k2", 2, "k1", 1), rec.Envelope)
		if err != nil {
			t.Fatal(err)
		}
		err = f.repo.UpdateEnvelope(ctx, secrets.UpdateEnvelopeParams{Name: "a", Old: rec.Envelope, New: newEnv})
		if !errors.Is(err, secrets.ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
	})

	// I14
	t.Run("I14 list pages", func(t *testing.T) {
		f := newFixture(t, pg)
		ctx := t.Context()
		svc := secrets.NewService(testRing(t, "k1", 1, "", 0), f.repo)
		for _, n := range []string{"a", "b", "c", "d", "e"} {
			if err := svc.Put(ctx, n, []byte("v-"+n)); err != nil {
				t.Fatal(err)
			}
		}
		var pages [][]string
		after := ""
		for {
			res, err := svc.List(ctx, secrets.ListParams{After: after, Limit: 2})
			if err != nil {
				t.Fatal(err)
			}
			pages = append(pages, res.Names)
			if res.Next == "" {
				break
			}
			after = res.Next
		}
		want := [][]string{{"a", "b"}, {"c", "d"}, {"e"}}
		if fmt.Sprint(pages) != fmt.Sprint(want) {
			t.Fatalf("pages = %v, want %v", pages, want)
		}
		// A list that ends exactly at a page end has an empty Next.
		res, err := svc.List(ctx, secrets.ListParams{After: "b", Limit: 3})
		if err != nil || res.Next != "" || fmt.Sprint(res.Names) != "[c d e]" {
			t.Fatalf("exact page = %+v, err %v", res, err)
		}
	})
}
