//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	clientpg "github.com/techorionai/constellation-platform-go/clients/postgres"
	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/migrate"
	migratepg "github.com/techorionai/constellation-platform-go/migrate/postgres"
	"github.com/techorionai/constellation-platform-go/resources/test/containers"
)

// testKey is the lock key that the plan gives: the FNV-1a hash of
// "constellation:migrate:" plus the name.
func testKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("constellation:migrate:" + name))
	return int64(h.Sum64()) //nolint:gosec // the hash value is the key
}

// source makes a migration source. The map key is the file name.
func source(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

// threeMigrations is the source for I1, I2, I5, I13, and I14.
func threeMigrations() fstest.MapFS {
	return source(map[string]string{
		"1_a.up.sql": "CREATE TABLE a (id int);",
		"2_b.up.sql": "CREATE TABLE b (id int);",
		"3_c.up.sql": "CREATE TABLE c (id int);",
	})
}

func newPool(t *testing.T, dbURL config.Secret, name string, maxConns int32) *pgxpool.Pool {
	t.Helper()
	c, err := clientpg.New(t.Context(), clientpg.Config{URL: dbURL, Database: name, MaxConns: maxConns})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c.Pool
}

func newMigrator(t *testing.T, pool *pgxpool.Pool, fsys fstest.MapFS, table string) *migratepg.Migrator {
	t.Helper()
	m, err := migratepg.NewMigrator(pool, fsys, migratepg.MigratorOptions{MigrationsTable: table})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func run(ctx context.Context, pool *pgxpool.Pool, m *migratepg.Migrator, name string, timeout time.Duration) (migrate.Result, error) {
	return migrate.Run(ctx, migratepg.NewLocker(pool, name), m, migrate.Options{Name: name, LockTimeout: timeout})
}

func connect(t *testing.T, dbURL config.Secret) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dbURL.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func queryInt(t *testing.T, conn *pgx.Conn, sql string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(t.Context(), sql).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// advisoryLocks counts the advisory locks in the database of conn.
func advisoryLocks(t *testing.T, conn *pgx.Conn) int {
	t.Helper()
	return queryInt(t, conn, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'
		AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`)
}

// pollQuery waits until another session of the database runs a statement that
// contains text. It gives up after 30 seconds.
func pollQuery(ctx context.Context, conn *pgx.Conn, text string) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		var n int
		err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
			AND state = 'active' AND query LIKE '%' || $1 || '%'`, text).Scan(&n)
		if err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no session runs a statement with %q", text)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForQuery is pollQuery for the test goroutine. It fails the test on an error.
func waitForQuery(t *testing.T, conn *pgx.Conn, text string) {
	t.Helper()
	if err := pollQuery(t.Context(), conn, text); err != nil {
		t.Fatal(err)
	}
}

// holdLock takes the advisory lock of the migration set name on a test
// connection. The returned function releases it.
func holdLock(t *testing.T, dbURL config.Secret, name string) (release func()) {
	t.Helper()
	conn := connect(t, dbURL)
	if _, err := conn.Exec(t.Context(), "SELECT pg_advisory_lock($1)", testKey(name)); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if _, err := conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", testKey(name)); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestIntegrationMigrate(t *testing.T) {
	pg := containers.Postgres(t)

	// I1 and I2
	t.Run("I1 I2 run twice", func(t *testing.T) {
		dbURL, name := pg.NewDatabase(t)
		pool := newPool(t, dbURL, name, 4)
		m := newMigrator(t, pool, threeMigrations(), "")
		res, err := run(t.Context(), pool, m, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		if res.Applied != 3 || res.ToVersion != 3 {
			t.Fatalf("result = %+v", res)
		}
		conn := connect(t, dbURL)
		if v := queryInt(t, conn, "SELECT version FROM schema_migrations"); v != 3 {
			t.Fatalf("version = %d", v)
		}
		for _, table := range []string{"a", "b", "c"} {
			if queryInt(t, conn, "SELECT count(*) FROM information_schema.tables WHERE table_name = '"+table+"'") != 1 {
				t.Fatalf("table %s is missing", table)
			}
		}
		res, err = run(t.Context(), pool, m, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		if res.Applied != 0 || res.Ahead {
			t.Fatalf("second result = %+v", res)
		}
		// I5
		if n := advisoryLocks(t, conn); n != 0 {
			t.Fatalf("advisory locks = %d", n)
		}
	})

	// I3
	t.Run("I3 two instances", func(t *testing.T) {
		dbURL, name := pg.NewDatabase(t)
		fsys := source(map[string]string{
			"1_a.up.sql": "CREATE TABLE applied_log (n int); INSERT INTO applied_log VALUES (1);",
			"2_b.up.sql": "SELECT pg_sleep(2); INSERT INTO applied_log VALUES (2);",
			"3_c.up.sql": "INSERT INTO applied_log VALUES (3);",
		})
		poolA, poolB := newPool(t, dbURL, name, 4), newPool(t, dbURL, name, 4)
		mA, mB := newMigrator(t, poolA, fsys, ""), newMigrator(t, poolB, fsys, "")
		type outcome struct {
			res migrate.Result
			err error
			d   time.Duration
		}
		out := make([]outcome, 2)
		var wg sync.WaitGroup
		for i, p := range []struct {
			pool *pgxpool.Pool
			m    *migratepg.Migrator
		}{{poolA, mA}, {poolB, mB}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				start := time.Now()
				res, err := run(t.Context(), p.pool, p.m, "", 0)
				out[i] = outcome{res, err, time.Since(start)}
			}()
		}
		wg.Wait()
		applied := 0
		for _, o := range out {
			if o.err != nil {
				t.Fatal(o.err)
			}
			if o.d < 2*time.Second {
				t.Fatalf("a call returned after %v", o.d)
			}
			applied += o.res.Applied
		}
		if applied != 3 {
			t.Fatalf("total applied = %d, want 3", applied)
		}
		conn := connect(t, dbURL)
		if n := queryInt(t, conn, "SELECT count(*) FROM applied_log"); n != 3 {
			t.Fatalf("applied_log rows = %d, want 3", n)
		}
	})

	// I4
	t.Run("I4 lock timeout", func(t *testing.T) {
		dbURL, name := pg.NewDatabase(t)
		pool := newPool(t, dbURL, name, 4)
		m := newMigrator(t, pool, threeMigrations(), "")
		release := holdLock(t, dbURL, "schema_migrations")
		start := time.Now()
		_, err := run(t.Context(), pool, m, "", time.Second)
		if !errors.Is(err, migrate.ErrLockTimeout) {
			t.Fatalf("err = %v, want ErrLockTimeout", err)
		}
		if d := time.Since(start); d < 900*time.Millisecond || d > 5*time.Second {
			t.Fatalf("Run took %v", d)
		}
		conn := connect(t, dbURL)
		if queryInt(t, conn, "SELECT count(*) FROM information_schema.tables WHERE table_name = 'schema_migrations'") != 0 {
			t.Fatal("the schema changed")
		}
		release()
		start = time.Now()
		if _, err := run(t.Context(), pool, m, "", 0); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("second Run took %v", d)
		}
		if n := advisoryLocks(t, conn); n != 0 {
			t.Fatalf("advisory locks = %d", n)
		}
		if n := pool.Stat().AcquiredConns(); n != 0 {
			t.Fatalf("acquired connections = %d", n)
		}
	})

	// I6
	t.Run("I6 failed migration", func(t *testing.T) {
		dbURL, name := pg.NewDatabase(t)
		pool := newPool(t, dbURL, name, 4)
		m := newMigrator(t, pool, source(map[string]string{
			"1_a.up.sql": "CREATE TABLE a (id int);",
			"2_b.up.sql": "CREATE TABEL b (id int);",
		}), "")
		if _, err := run(t.Context(), pool, m, "", 0); err == nil {
			t.Fatal("want error")
		}
		_, err := run(t.Context(), pool, m, "", 0)
		if !errors.Is(err, migrate.ErrDirty) {
			t.Fatalf("err = %v, want ErrDirty", err)
		}
	})

	// I7
	t.Run("I7 two tables", func(t *testing.T) {
		dbURL, name := pg.NewDatabase(t)
		pool := newPool(t, dbURL, name, 4)
		mA := newMigrator(t, pool, source(map[string]string{"1_a.up.sql": "CREATE TABLE a1 (id int);"}), "a_migrations")
		mB := newMigrator(t, pool, source(map[string]string{
			"1_b.up.sql": "CREATE TABLE b1 (id int);",
			"2_b.up.sql": "CREATE TABLE b2 (id int);",
		}), "b_migrations")
		if testKey("a_migrations") == testKey("b_migrations") {
			t.Fatal("the lock keys are the same")
		}
		release := holdLock(t, dbURL, "a_migrations")
		defer release()
		// The lock of set a does not block set b.
		if _, err := run(t.Context(), pool, mB, "b_migrations", time.Second); err != nil {
			t.Fatal(err)
		}
		release()
		if _, err := run(t.Context(), pool, mA, "a_migrations", time.Second); err != nil {
			t.Fatal(err)
		}
		conn := connect(t, dbURL)
		if v := queryInt(t, conn, "SELECT version FROM a_migrations"); v != 1 {
			t.Fatalf("a version = %d", v)
		}
		if v := queryInt(t, conn, "SELECT version FROM b_migrations"); v != 2 {
			t.Fatalf("b version = %d", v)
		}
	})

	// I11
	t.Run("I11 database from config", func(t *testing.T) {
		pg.Exec(t, "CREATE DATABASE a")
		t.Cleanup(func() { pg.Exec(t, "DROP DATABASE IF EXISTS a WITH (FORCE)") })
		u, err := url.Parse(pg.AdminURL().Reveal())
		if err != nil {
			t.Fatal(err)
		}
		u.Path = ""
		client, err := clientpg.New(t.Context(), clientpg.Config{URL: config.Secret(u.String()), Database: "a"})
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		m := newMigrator(t, client.Pool, threeMigrations(), "")
		if _, err := run(t.Context(), client.Pool, m, "", 0); err != nil {
			t.Fatal(err)
		}
		var inA, inDefault *string
		if err := client.Pool.QueryRow(t.Context(), "SELECT to_regclass('public.schema_migrations')::text").Scan(&inA); err != nil {
			t.Fatal(err)
		}
		if inA == nil {
			t.Fatal("schema_migrations is missing in database a")
		}
		conn := connect(t, pg.AdminURL())
		if err := conn.QueryRow(t.Context(), "SELECT to_regclass('public.schema_migrations')::text").Scan(&inDefault); err != nil {
			t.Fatal(err)
		}
		if inDefault != nil {
			t.Fatal("schema_migrations exists in the default database")
		}
	})

	// I12
	t.Run("I12 connections", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			files map[string]string
			fail  bool
		}{
			{"ok", map[string]string{"1_a.up.sql": "CREATE TABLE a (id int);"}, false},
			{"syntax error", map[string]string{"1_a.up.sql": "CREATE TABEL a (id int);"}, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dbURL, name := pg.NewDatabase(t)
				pool := newPool(t, dbURL, name, 4)
				before := pool.Stat().TotalConns()
				m := newMigrator(t, pool, source(tc.files), "")
				if after := pool.Stat().TotalConns(); after > before {
					t.Fatalf("NewMigrator opened a connection: %d to %d", before, after)
				}
				_, err := run(t.Context(), pool, m, "", 0)
				if (err != nil) != tc.fail {
					t.Fatalf("err = %v", err)
				}
				if n := pool.Stat().AcquiredConns(); n != 0 {
					t.Fatalf("acquired connections = %d", n)
				}
				var one int
				if err := pool.QueryRow(t.Context(), "SELECT 1").Scan(&one); err != nil {
					t.Fatalf("the pool does not work: %v", err)
				}
			})
		}
	})

	// I13
	t.Run("I13 database ahead", func(t *testing.T) {
		dbURL, name := pg.NewDatabase(t)
		pool := newPool(t, dbURL, name, 4)
		if _, err := run(t.Context(), pool, newMigrator(t, pool, threeMigrations(), ""), "", 0); err != nil {
			t.Fatal(err)
		}
		old := source(map[string]string{"1_a.up.sql": "CREATE TABLE a (id int);", "2_b.up.sql": "CREATE TABLE b (id int);"})
		res, err := run(t.Context(), pool, newMigrator(t, pool, old, ""), "", 0)
		if err != nil {
			t.Fatal(err)
		}
		if res.Applied != 0 || !res.Ahead {
			t.Fatalf("result = %+v", res)
		}
		conn := connect(t, dbURL)
		if v := queryInt(t, conn, "SELECT version FROM schema_migrations"); v != 3 {
			t.Fatalf("version = %d", v)
		}
	})

	// I14
	t.Run("I14 canceled", func(t *testing.T) {
		dbURL, name := pg.NewDatabase(t)
		pool := newPool(t, dbURL, name, 4)
		m := newMigrator(t, pool, source(map[string]string{
			"1_a.up.sql": "CREATE TABLE a (id int);",
			"2_b.up.sql": "SELECT pg_sleep(2); CREATE TABLE b (id int);",
			"3_c.up.sql": "CREATE TABLE c (id int);",
		}), "")
		ctx, cancel := context.WithCancel(t.Context())
		watch := connect(t, dbURL)
		go func() {
			// Cancel when migration 2 runs, not after a fixed time.
			if err := pollQuery(t.Context(), watch, "pg_sleep(2)"); err != nil {
				t.Error(err)
			}
			cancel()
		}()
		_, err := run(ctx, pool, m, "", 0)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		conn := connect(t, dbURL)
		var version int
		var dirty bool
		if err := conn.QueryRow(t.Context(), "SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty); err != nil {
			t.Fatal(err)
		}
		if version != 2 || dirty {
			t.Fatalf("version = %d, dirty = %v", version, dirty)
		}
		if queryInt(t, conn, "SELECT count(*) FROM information_schema.tables WHERE table_name = 'c'") != 0 {
			t.Fatal("migration 3 was applied")
		}
		if n := advisoryLocks(t, conn); n != 0 {
			t.Fatalf("advisory locks = %d", n)
		}
	})

	// I15
	t.Run("I15 long wait", func(t *testing.T) {
		if testing.Short() {
			t.Skip("takes 20 seconds")
		}
		dbURL, name := pg.NewDatabase(t)
		pool := newPool(t, dbURL, name, 4)
		m := newMigrator(t, pool, threeMigrations(), "")
		release := holdLock(t, dbURL, "schema_migrations")
		time.AfterFunc(20*time.Second, release)
		start := time.Now()
		res, err := run(t.Context(), pool, m, "", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d < 20*time.Second {
			t.Fatalf("Run returned after %v", d)
		}
		if res.Applied != 3 {
			t.Fatalf("result = %+v", res)
		}
	})

	// I16
	t.Run("I16 no privilege", func(t *testing.T) {
		dbURL, name := pg.NewDatabase(t)
		admin := connect(t, dbURL)
		for _, sql := range []string{
			"CREATE ROLE limited LOGIN PASSWORD 'limited-pass-123'",
			"REVOKE CREATE ON SCHEMA public FROM PUBLIC",
			"CREATE TABLE schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)",
			"GRANT ALL ON schema_migrations TO limited",
		} {
			if _, err := admin.Exec(t.Context(), sql); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		u, err := url.Parse(dbURL.Reveal())
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword("limited", "limited-pass-123")
		pool := newPool(t, config.Secret(u.String()), name, 4)
		m := newMigrator(t, pool, source(map[string]string{"1_a.up.sql": "CREATE TABLE t1 (id int);"}), "")
		_, err = run(t.Context(), pool, m, "", 0)
		if err == nil {
			t.Fatal("want error")
		}
		if strings.Contains(err.Error(), "limited-pass-123") {
			t.Fatalf("the error has the password: %v", err)
		}
		if n := advisoryLocks(t, admin); n != 0 {
			t.Fatalf("advisory locks = %d", n)
		}
	})

	// I19
	t.Run("I19 second instance canceled", func(t *testing.T) {
		dbURL, name := pg.NewDatabase(t)
		fsys := source(map[string]string{
			"1_a.up.sql": "SELECT pg_sleep(5); CREATE TABLE a (id int);",
			"2_b.up.sql": "CREATE TABLE b (id int);",
		})
		poolA, poolB := newPool(t, dbURL, name, 4), newPool(t, dbURL, name, 4)
		mA := newMigrator(t, poolA, fsys, "")
		errA := make(chan error, 1)
		go func() {
			_, err := run(t.Context(), poolA, mA, "", 0)
			errA <- err
		}()
		// Wait until A holds the lock and runs migration 1.
		waitForQuery(t, connect(t, dbURL), "pg_sleep(5)")

		start := time.Now()
		mB := newMigrator(t, poolB, fsys, "")
		if d := time.Since(start); d > 100*time.Millisecond {
			t.Fatalf("NewMigrator took %v", d)
		}
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(time.Second, cancel)
		start = time.Now()
		_, err := run(ctx, poolB, mB, "", 0)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err of B = %v, want context.Canceled", err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("Run of B took %v", d)
		}
		if err := <-errA; err != nil {
			t.Fatalf("err of A = %v", err)
		}
		conn := connect(t, dbURL)
		if v := queryInt(t, conn, "SELECT version FROM schema_migrations"); v != 2 {
			t.Fatalf("version = %d", v)
		}
	})

	// I20
	t.Run("I20 pool size", func(t *testing.T) {
		dbURL, name := pg.NewDatabase(t)
		small := newPool(t, dbURL, name, 1)
		before := small.Stat().TotalConns()
		_, err := migratepg.NewMigrator(small, threeMigrations(), migratepg.MigratorOptions{})
		if !errors.Is(err, migratepg.ErrPoolTooSmall) {
			t.Fatalf("err = %v, want ErrPoolTooSmall", err)
		}
		if after := small.Stat().TotalConns(); after > before {
			t.Fatalf("a connection was opened: %d to %d", before, after)
		}
		pool := newPool(t, dbURL, name, 2)
		m := newMigrator(t, pool, threeMigrations(), "")
		if _, err := run(t.Context(), pool, m, "", 0); err != nil {
			t.Fatal(err)
		}
	})
}
