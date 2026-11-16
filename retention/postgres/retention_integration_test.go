//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"hash/fnv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	clientpg "github.com/techorionai/constellation-platform-go/clients/postgres"
	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/resources/test/containers"
	"github.com/techorionai/constellation-platform-go/retention"
	retpg "github.com/techorionai/constellation-platform-go/retention/postgres"
)

// lockKey is the key that the plan gives: the FNV-1a hash of "constellation:"
// plus the lock name.
func lockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("constellation:" + name))
	return int64(h.Sum64()) //nolint:gosec // the hash value is the key
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

func connect(t *testing.T, dbURL config.Secret) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dbURL.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func count(t *testing.T, conn interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, sql string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(t.Context(), sql).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// newRunner makes a runner for the job "events" with the table purger.
func newRunner(t *testing.T, pool *pgxpool.Pool, batch int, spec retpg.TableSpec) *retention.Runner {
	t.Helper()
	purger, err := retpg.NewTablePurger(pool, spec)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := retpg.NewLocker(pool)
	if err != nil {
		t.Fatal(err)
	}
	r, err := retention.NewRunner(locker, []retention.Job{{
		Name:   "events",
		Config: retention.Config{Period: 72 * time.Hour, Interval: time.Hour, BatchSize: batch},
		Purger: purger,
	}}, retention.RunnerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func advisoryLocks(t *testing.T, conn *pgx.Conn) int {
	return count(t, conn, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'
		AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`)
}

func TestIntegrationRetention(t *testing.T) {
	pg := containers.Postgres(t)
	spec := retpg.TableSpec{Table: "events", TimeColumn: "created_at"}

	// I8 and I11
	t.Run("I8 I11 batches", func(t *testing.T) {
		dbURL, name := pg.NewDatabase(t)
		pool := newPool(t, dbURL, name, 4)
		exec(t, pool, `CREATE TABLE events (id bigserial PRIMARY KEY, created_at timestamptz NOT NULL)`)
		exec(t, pool, `INSERT INTO events (created_at) SELECT now() - interval '10 days' FROM generate_series(1, 2500)`)
		exec(t, pool, `INSERT INTO events (created_at) SELECT now() FROM generate_series(1, 10)`)
		r := newRunner(t, pool, 1000, spec)
		n, err := r.RunOnce(t.Context(), "events")
		if err != nil || n != 2500 {
			t.Fatalf("n %d, err %v", n, err)
		}
		if left := count(t, pool, `SELECT count(*) FROM events`); left != 10 {
			t.Fatalf("%d rows remain, want 10", left)
		}
		// I11
		if n := advisoryLocks(t, connect(t, dbURL)); n != 0 {
			t.Fatalf("advisory locks = %d", n)
		}
	})

	// I15
	t.Run("I15 partitioned table", func(t *testing.T) {
		dbURL, name := pg.NewDatabase(t)
		pool := newPool(t, dbURL, name, 4)
		exec(t, pool, `CREATE TABLE events (id bigint NOT NULL, created_at timestamptz NOT NULL, PRIMARY KEY (id, created_at))
			PARTITION BY RANGE (created_at)`)
		exec(t, pool, `CREATE TABLE p_old PARTITION OF events FOR VALUES FROM ('2000-01-01') TO ('2020-01-01')`)
		exec(t, pool, `CREATE TABLE p_new PARTITION OF events FOR VALUES FROM ('2020-01-01') TO ('2200-01-01')`)
		exec(t, pool, `INSERT INTO events SELECT g, '2010-06-01'::timestamptz FROM generate_series(1, 50) g`)
		exec(t, pool, `INSERT INTO events SELECT g, now() FROM generate_series(1, 50) g`)
		// The same ctid exists in both partitions.
		if n := count(t, pool, `SELECT count(*) FROM p_old o JOIN p_new n ON o.ctid = n.ctid AND o.id = n.id`); n != 50 {
			t.Fatalf("shared ctid rows = %d, want 50", n)
		}
		r := newRunner(t, pool, 20, retpg.TableSpec{Table: "events", TimeColumn: "created_at", KeyColumn: "id"})
		n, err := r.RunOnce(t.Context(), "events")
		if err != nil || n != 50 {
			t.Fatalf("n %d, err %v", n, err)
		}
		if old, fresh := count(t, pool, `SELECT count(*) FROM p_old`), count(t, pool, `SELECT count(*) FROM p_new`); old != 0 || fresh != 50 {
			t.Fatalf("p_old %d rows, p_new %d rows, want 0 and 50", old, fresh)
		}
	})

	// I9
	t.Run("I9 two runners and a lock", func(t *testing.T) {
		dbURL, name := pg.NewDatabase(t)
		poolA, poolB := newPool(t, dbURL, name, 4), newPool(t, dbURL, name, 4)
		exec(t, poolA, `CREATE TABLE events (id bigserial PRIMARY KEY, created_at timestamptz NOT NULL)`)
		exec(t, poolA, `INSERT INTO events (created_at) SELECT now() - interval '10 days' FROM generate_series(1, 30)`)
		holder := connect(t, dbURL)
		if _, err := holder.Exec(t.Context(), "SELECT pg_advisory_lock($1)", lockKey("retention:events")); err != nil {
			t.Fatal(err)
		}
		for _, p := range []*pgxpool.Pool{poolA, poolB} {
			n, err := newRunner(t, p, 10, spec).RunOnce(t.Context(), "events")
			if n != 0 || !errors.Is(err, retention.ErrLocked) {
				t.Fatalf("n %d, err %v", n, err)
			}
		}
		if left := count(t, poolA, `SELECT count(*) FROM events`); left != 30 {
			t.Fatalf("%d rows remain, want 30", left)
		}
		// After the release, a runner deletes the rows.
		if _, err := holder.Exec(t.Context(), "SELECT pg_advisory_unlock($1)", lockKey("retention:events")); err != nil {
			t.Fatal(err)
		}
		if n, err := newRunner(t, poolB, 10, spec).RunOnce(t.Context(), "events"); err != nil || n != 30 {
			t.Fatalf("n %d, err %v", n, err)
		}
	})

	// I10
	t.Run("I10 one lock owner", func(t *testing.T) {
		dbURL, name := pg.NewDatabase(t)
		pool := newPool(t, dbURL, name, 8)
		locker, err := retpg.NewLocker(pool)
		if err != nil {
			t.Fatal(err)
		}
		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			acquired int
			unlocks  []func(context.Context) error
		)
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				unlock, ok, err := locker.TryLock(t.Context(), "retention:x")
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				if ok {
					acquired++
					unlocks = append(unlocks, unlock)
				}
			}()
		}
		wg.Wait()
		if acquired != 1 {
			t.Fatalf("%d owners, want 1", acquired)
		}
		if err := unlocks[0](t.Context()); err != nil {
			t.Fatal(err)
		}
		// The lock is free again.
		unlock, ok, err := locker.TryLock(t.Context(), "retention:x")
		if err != nil || !ok {
			t.Fatalf("ok %v, err %v", ok, err)
		}
		if err := unlock(t.Context()); err != nil {
			t.Fatal(err)
		}
	})

	// I16
	t.Run("I16 pool size", func(t *testing.T) {
		dbURL, name := pg.NewDatabase(t)
		small := newPool(t, dbURL, name, 1)
		before := small.Stat().AcquireCount()
		if _, err := retpg.NewLocker(small); !errors.Is(err, retpg.ErrPoolTooSmall) {
			t.Fatalf("err = %v, want ErrPoolTooSmall", err)
		}
		if after := small.Stat().AcquireCount(); after != before {
			t.Fatalf("NewLocker acquired a connection (%d to %d)", before, after)
		}

		pool := newPool(t, dbURL, name, 2)
		exec(t, pool, `CREATE TABLE events (id bigserial PRIMARY KEY, created_at timestamptz NOT NULL)`)
		exec(t, pool, `INSERT INTO events (created_at) SELECT now() - interval '10 days' FROM generate_series(1, 10)`)
		n, err := newRunner(t, pool, 100, spec).RunOnce(t.Context(), "events")
		if err != nil || n != 10 {
			t.Fatalf("n %d, err %v", n, err)
		}
	})
}
