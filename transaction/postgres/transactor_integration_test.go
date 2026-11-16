//go:build integration

package postgres_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	clientpg "github.com/techorionai/constellation-platform-go/clients/postgres"
	"github.com/techorionai/constellation-platform-go/resources/test/containers"
	"github.com/techorionai/constellation-platform-go/transaction"
	txpg "github.com/techorionai/constellation-platform-go/transaction/postgres"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type env struct {
	pool   *pgxpool.Pool
	reader *sdkmetric.ManualReader
	logs   *lockedBuffer
}

// newEnv makes a database, a pool, a metric reader, and a log buffer. The
// global providers are set before the pool and the transactor are made.
func newEnv(t *testing.T, pg *containers.PostgresInstance, maxConns int32) *env {
	t.Helper()
	oldMP, oldLog := otel.GetMeterProvider(), slog.Default()
	e := &env{reader: sdkmetric.NewManualReader(), logs: &lockedBuffer{}}
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(e.reader)))
	slog.SetDefault(slog.New(slog.NewTextHandler(e.logs, nil)))
	t.Cleanup(func() { otel.SetMeterProvider(oldMP); slog.SetDefault(oldLog) })

	dbURL, name := pg.NewDatabase(t)
	c, err := clientpg.New(t.Context(), clientpg.Config{URL: dbURL, Database: name, MaxConns: maxConns})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	e.pool = c.Pool
	if _, err := e.pool.Exec(t.Context(), `CREATE TABLE items (id text PRIMARY KEY, v int NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) exists(t *testing.T, id string) bool {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(t.Context(), `SELECT count(*) FROM items WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

type point struct {
	attrs map[string]string
	count uint64
	value int64
	sum   float64
	bound []float64
}

func (p point) has(kv ...string) bool {
	for i := 0; i < len(kv); i += 2 {
		if p.attrs[kv[i]] != kv[i+1] {
			return false
		}
	}
	return true
}

func (e *env) metrics(t *testing.T) map[string][]point {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := e.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	conv := func(s attribute.Set) map[string]string {
		m := map[string]string{}
		for _, kv := range s.ToSlice() {
			m[string(kv.Key)] = kv.Value.String()
		}
		return m
	}
	out := map[string][]point{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					out[m.Name] = append(out[m.Name], point{attrs: conv(dp.Attributes), value: dp.Value})
				}
			case metricdata.Histogram[float64]:
				for _, dp := range d.DataPoints {
					out[m.Name] = append(out[m.Name], point{attrs: conv(dp.Attributes), count: dp.Count, sum: dp.Sum, bound: dp.Bounds})
				}
			}
		}
	}
	return out
}

func count(pts []point, kv ...string) (n uint64) {
	for _, p := range pts {
		if p.has(kv...) {
			n += p.count
		}
	}
	return n
}

func value(pts []point, kv ...string) (n int64) {
	for _, p := range pts {
		if p.has(kv...) {
			n += p.value
		}
	}
	return n
}

func insert(ctx context.Context, pool *pgxpool.Pool, id string) error {
	_, err := clientpg.Conn(ctx, pool).Exec(ctx, `INSERT INTO items (id) VALUES ($1)`, id)
	return err
}

var errX = errors.New("x")

func TestIntegrationTransactor(t *testing.T) {
	pg := containers.Postgres(t)

	// I6
	t.Run("I6 commit", func(t *testing.T) {
		e := newEnv(t, pg, 5)
		tx := txpg.NewTransactor(e.pool)
		if err := tx.WithinTx(t.Context(), func(ctx context.Context) error { return insert(ctx, e.pool, "a") }); err != nil {
			t.Fatal(err)
		}
		if !e.exists(t, "a") {
			t.Fatal("the row does not exist")
		}
	})

	// I7
	t.Run("I7 rollback on error", func(t *testing.T) {
		e := newEnv(t, pg, 5)
		tx := txpg.NewTransactor(e.pool)
		err := tx.WithinTx(t.Context(), func(ctx context.Context) error {
			if err := insert(ctx, e.pool, "a"); err != nil {
				return err
			}
			return errX
		})
		if !errors.Is(err, errX) {
			t.Fatalf("err = %v", err)
		}
		if e.exists(t, "a") {
			t.Fatal("the row exists")
		}
	})

	// I8, I15 (the active count after a panic)
	t.Run("I8 rollback on panic", func(t *testing.T) {
		e := newEnv(t, pg, 5)
		tx := txpg.NewTransactor(e.pool)
		func() {
			defer func() {
				if r := recover(); r != "boom" {
					t.Fatalf("recover = %v", r)
				}
			}()
			_ = tx.WithinTx(t.Context(), func(ctx context.Context) error {
				if err := insert(ctx, e.pool, "a"); err != nil {
					return err
				}
				panic("boom")
			})
		}()
		if e.exists(t, "a") {
			t.Fatal("the row exists")
		}
		if n := e.pool.Stat().AcquiredConns(); n != 0 {
			t.Fatalf("acquired conns = %d", n)
		}
		if n := value(e.metrics(t)["constellation.db.transaction.active"]); n != 0 {
			t.Fatalf("active = %d after a panic", n)
		}
	})

	// I9
	t.Run("I9 nested rollback", func(t *testing.T) {
		e := newEnv(t, pg, 5)
		tx := txpg.NewTransactor(e.pool)
		err := tx.WithinTx(t.Context(), func(ctx context.Context) error {
			if err := insert(ctx, e.pool, "a"); err != nil {
				return err
			}
			if err := tx.WithinTx(ctx, func(ctx context.Context) error { return insert(ctx, e.pool, "b") }); err != nil {
				return err
			}
			return errX
		})
		if !errors.Is(err, errX) {
			t.Fatalf("err = %v", err)
		}
		if e.exists(t, "a") || e.exists(t, "b") {
			t.Fatal("a row exists")
		}
	})

	// I10
	t.Run("I10 nested options", func(t *testing.T) {
		e := newEnv(t, pg, 5)
		tx := txpg.NewTransactor(e.pool)
		err := tx.WithinTx(t.Context(), func(ctx context.Context) error {
			return tx.WithinTx(ctx, func(context.Context) error { return nil }, transaction.ReadOnly())
		})
		if !errors.Is(err, transaction.ErrNestedOptions) {
			t.Fatalf("err = %v", err)
		}
	})

	// I11
	t.Run("I11 read only and isolation", func(t *testing.T) {
		e := newEnv(t, pg, 5)
		tx := txpg.NewTransactor(e.pool)
		err := tx.WithinTx(t.Context(), func(ctx context.Context) error { return insert(ctx, e.pool, "a") }, transaction.ReadOnly())
		if err == nil {
			t.Fatal("the insert in a read-only transaction must fail")
		}
		var level string
		err = tx.WithinTx(t.Context(), func(ctx context.Context) error {
			return clientpg.Conn(ctx, e.pool).QueryRow(ctx, `SHOW transaction_isolation`).Scan(&level)
		}, transaction.Isolation(transaction.Serializable))
		if err != nil || level != "serializable" {
			t.Fatalf("level %q, err %v", level, err)
		}
	})

	// conflict runs two serializable transactions in a write skew. T1 commits
	// first. The commit of T2 fails. hook2 is registered in T2 (if not nil).
	conflict := func(t *testing.T, e *env, tx *txpg.Transactor, hook2 func(context.Context)) (err1, err2 error) {
		t.Helper()
		if _, err := e.pool.Exec(t.Context(), `INSERT INTO items (id, v) VALUES ('A', 1), ('B', 1)`); err != nil {
			t.Fatal(err)
		}
		t2updated := make(chan struct{})
		t1done := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			defer close(t1done)
			err1 = tx.WithinTx(t.Context(), func(ctx context.Context) error {
				q := clientpg.Conn(ctx, e.pool)
				var s int
				if err := q.QueryRow(ctx, `SELECT sum(v) FROM items`).Scan(&s); err != nil {
					return err
				}
				if _, err := q.Exec(ctx, `UPDATE items SET v = v + 1 WHERE id = 'A'`); err != nil {
					return err
				}
				<-t2updated
				return nil
			}, transaction.Isolation(transaction.Serializable), transaction.Name("t1"))
		}()
		go func() {
			defer wg.Done()
			err2 = tx.WithinTx(t.Context(), func(ctx context.Context) error {
				q := clientpg.Conn(ctx, e.pool)
				var s int
				if err := q.QueryRow(ctx, `SELECT sum(v) FROM items`).Scan(&s); err != nil {
					close(t2updated)
					return err
				}
				if _, err := q.Exec(ctx, `UPDATE items SET v = v + 1 WHERE id = 'B'`); err != nil {
					close(t2updated)
					return err
				}
				if hook2 != nil {
					tx.AfterCommit(ctx, hook2)
				}
				close(t2updated)
				<-t1done
				return nil
			}, transaction.Isolation(transaction.Serializable), transaction.Name("t2"))
		}()
		wg.Wait()
		return err1, err2
	}

	// I13, I16
	t.Run("I13 I16 serialization failure", func(t *testing.T) {
		e := newEnv(t, pg, 5)
		tx := txpg.NewTransactor(e.pool)
		err1, err2 := conflict(t, e, tx, nil)
		if err1 != nil {
			t.Fatalf("T1: %v", err1)
		}
		if !errors.Is(err2, transaction.ErrSerialization) {
			t.Fatalf("T2: %v", err2)
		}
		m := e.metrics(t)
		d := m["constellation.db.transaction.duration"]
		if count(d, "db.transaction.name", "t1", "outcome", "commit") != 1 ||
			count(d, "db.transaction.name", "t2", "outcome", "commit_error") != 1 {
			t.Fatalf("duration: %+v", d)
		}
		if value(m["constellation.db.transaction.serialization_failures"], "sqlstate", "40001") != 1 {
			t.Fatalf("serialization_failures: %+v", m["constellation.db.transaction.serialization_failures"])
		}
	})

	// I14
	t.Run("I14 duration outcomes", func(t *testing.T) {
		e := newEnv(t, pg, 5)
		tx := txpg.NewTransactor(e.pool)
		name := transaction.Name("create_order")
		_ = tx.WithinTx(t.Context(), func(context.Context) error { return nil }, name)
		_ = tx.WithinTx(t.Context(), func(context.Context) error { return errX }, name)
		func() {
			defer func() { _ = recover() }()
			_ = tx.WithinTx(t.Context(), func(context.Context) error { panic("p") }, name)
		}()
		m := e.metrics(t)
		d := m["constellation.db.transaction.duration"]
		for _, o := range []string{"commit", "rollback", "panic"} {
			if count(d, "db.transaction.name", "create_order", "outcome", o) != 1 {
				t.Fatalf("outcome %s: %+v", o, d)
			}
		}
		if count(m["constellation.db.transaction.begin.duration"]) != 3 {
			t.Fatalf("begin.duration: %+v", m["constellation.db.transaction.begin.duration"])
		}
		want := fmt.Sprint([]float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10})
		for _, name := range []string{"constellation.db.transaction.duration", "constellation.db.transaction.begin.duration"} {
			if fmt.Sprint(m[name][0].bound) != want {
				t.Fatalf("%s bounds = %v", name, m[name][0].bound)
			}
		}
	})

	// I15
	t.Run("I15 active count", func(t *testing.T) {
		e := newEnv(t, pg, 5)
		tx := txpg.NewTransactor(e.pool)
		var inside int64
		err := tx.WithinTx(t.Context(), func(context.Context) error {
			inside = value(e.metrics(t)["constellation.db.transaction.active"])
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if inside != 1 {
			t.Fatalf("active in fn = %d", inside)
		}
		if n := value(e.metrics(t)["constellation.db.transaction.active"]); n != 0 {
			t.Fatalf("active after = %d", n)
		}
	})

	// I17
	t.Run("I17 nested counter", func(t *testing.T) {
		e := newEnv(t, pg, 5)
		tx := txpg.NewTransactor(e.pool)
		err := tx.WithinTx(t.Context(), func(ctx context.Context) error {
			return tx.WithinTx(ctx, func(context.Context) error { return nil }, transaction.Name("inner"))
		})
		if err != nil {
			t.Fatal(err)
		}
		m := e.metrics(t)
		if value(m["constellation.db.transaction.nested"], "db.transaction.name", "inner") != 1 {
			t.Fatalf("nested: %+v", m["constellation.db.transaction.nested"])
		}
		if count(m["constellation.db.transaction.duration"]) != 1 {
			t.Fatalf("duration: %+v", m["constellation.db.transaction.duration"])
		}
	})

	// I18
	t.Run("I18 begin waits for a connection", func(t *testing.T) {
		e := newEnv(t, pg, 1)
		tx := txpg.NewTransactor(e.pool)
		held, err := e.pool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		go func() { time.Sleep(200 * time.Millisecond); held.Release() }()
		if err := tx.WithinTx(t.Context(), func(context.Context) error { return nil }); err != nil {
			t.Fatal(err)
		}
		first := e.metrics(t)["constellation.db.transaction.begin.duration"]
		if len(first) != 1 || first[0].sum < 0.19 {
			t.Fatalf("begin.duration: %+v", first)
		}

		held, err = e.pool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer held.Release()
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		err = tx.WithinTx(ctx, func(context.Context) error { return nil })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
		if count(e.metrics(t)["constellation.db.transaction.duration"], "outcome", "begin_error") != 1 {
			t.Fatalf("duration: %+v", e.metrics(t)["constellation.db.transaction.duration"])
		}
	})

	// R2
	t.Run("R2 Goexit in fn", func(t *testing.T) {
		e := newEnv(t, pg, 5)
		tx := txpg.NewTransactor(e.pool)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = tx.WithinTx(context.Background(), func(ctx context.Context) error {
				if err := insert(ctx, e.pool, "a"); err != nil {
					return err
				}
				runtime.Goexit()
				return nil
			}, transaction.Name("goexit_case"))
		}()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("the goroutine did not end")
		}
		if e.exists(t, "a") {
			t.Fatal("the row exists")
		}
		if n := e.pool.Stat().AcquiredConns(); n != 0 {
			t.Fatalf("acquired conns = %d", n)
		}
		m := e.metrics(t)
		if n := value(m["constellation.db.transaction.active"]); n != 0 {
			t.Fatalf("active = %d after Goexit", n)
		}
		if count(m["constellation.db.transaction.duration"], "outcome", "goexit") != 1 {
			t.Fatalf("duration: %+v", m["constellation.db.transaction.duration"])
		}
	})

	// R3
	t.Run("R3 cancel after fn does not stop the commit", func(t *testing.T) {
		e := newEnv(t, pg, 5)
		tx := txpg.NewTransactor(e.pool)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var hookRan bool
		err := tx.WithinTx(ctx, func(ctx context.Context) error {
			if err := insert(ctx, e.pool, "a"); err != nil {
				return err
			}
			tx.AfterCommit(ctx, func(context.Context) { hookRan = true })
			cancel()
			return nil
		})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !e.exists(t, "a") {
			t.Fatal("the row does not exist")
		}
		if !hookRan {
			t.Fatal("the hook did not run")
		}
	})

	// I24
	t.Run("I24 hooks run after the commit in order", func(t *testing.T) {
		e := newEnv(t, pg, 5)
		tx := txpg.NewTransactor(e.pool)
		var order []string
		hook := func(name string) func(context.Context) {
			return func(ctx context.Context) {
				order = append(order, fmt.Sprintf("%s:%v", name, e.exists(t, "a")))
			}
		}
		err := tx.WithinTx(t.Context(), func(ctx context.Context) error {
			if err := insert(ctx, e.pool, "a"); err != nil {
				return err
			}
			tx.AfterCommit(ctx, hook("h1"))
			tx.AfterCommit(ctx, hook("h2"))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(order) != "[h1:true h2:true]" {
			t.Fatalf("order = %v", order)
		}
	})

	// I25
	t.Run("I25 hooks do not run after a rollback or a failed commit", func(t *testing.T) {
		e := newEnv(t, pg, 5)
		tx := txpg.NewTransactor(e.pool)
		var ran atomic.Int32
		hook := func(context.Context) { ran.Add(1) }
		err := tx.WithinTx(t.Context(), func(ctx context.Context) error {
			tx.AfterCommit(ctx, hook)
			return errX
		})
		if !errors.Is(err, errX) || ran.Load() != 0 {
			t.Fatalf("err %v, ran %d", err, ran.Load())
		}
		_, err2 := conflict(t, e, tx, hook)
		if !errors.Is(err2, transaction.ErrSerialization) || ran.Load() != 0 {
			t.Fatalf("err %v, ran %d", err2, ran.Load())
		}
	})

	// I26
	t.Run("I26 nested hook runs after the outer fn", func(t *testing.T) {
		e := newEnv(t, pg, 5)
		tx := txpg.NewTransactor(e.pool)
		var fnDone atomic.Bool
		var ran, ranAfterFn, visible atomic.Int32
		err := tx.WithinTx(t.Context(), func(ctx context.Context) error {
			if err := tx.WithinTx(ctx, func(ctx context.Context) error {
				if err := insert(ctx, e.pool, "a"); err != nil {
					return err
				}
				tx.AfterCommit(ctx, func(context.Context) {
					ran.Add(1)
					if fnDone.Load() {
						ranAfterFn.Add(1)
					}
					if e.exists(t, "a") {
						visible.Add(1)
					}
				})
				return nil
			}); err != nil {
				return err
			}
			fnDone.Store(true)
			return nil
		})
		if err != nil || ran.Load() != 1 || ranAfterFn.Load() != 1 || visible.Load() != 1 {
			t.Fatalf("err %v, ran %d, after fn %d, visible %d", err, ran.Load(), ranAfterFn.Load(), visible.Load())
		}
	})

	// I27
	t.Run("I27 a panic in a hook", func(t *testing.T) {
		e := newEnv(t, pg, 5)
		tx := txpg.NewTransactor(e.pool)
		second := false
		err := tx.WithinTx(t.Context(), func(ctx context.Context) error {
			tx.AfterCommit(ctx, func(context.Context) { panic("hook") })
			tx.AfterCommit(ctx, func(context.Context) { second = true })
			return nil
		})
		if err != nil || !second {
			t.Fatalf("err %v, second %v", err, second)
		}
		if n := strings.Count(e.logs.String(), "level=ERROR"); n != 1 {
			t.Fatalf("error records = %d: %s", n, e.logs.String())
		}
	})

	// I28
	t.Run("I28 AfterCommit without a transaction", func(t *testing.T) {
		e := newEnv(t, pg, 5)
		tx := txpg.NewTransactor(e.pool)
		ran := false
		tx.AfterCommit(t.Context(), func(context.Context) { ran = true })
		if !ran {
			t.Fatal("the hook did not run")
		}
	})

	// I29
	t.Run("I29 invalid option", func(t *testing.T) {
		e := newEnv(t, pg, 5)
		tx := txpg.NewTransactor(e.pool)
		called := false
		err := tx.WithinTx(t.Context(), func(context.Context) error { called = true; return nil }, transaction.Name("Bad-Name"))
		if !errors.Is(err, transaction.ErrInvalidOption) || called {
			t.Fatalf("err %v, called %v", err, called)
		}
		if n := count(e.metrics(t)["constellation.db.transaction.begin.duration"]); n != 0 {
			t.Fatalf("begin.duration count = %d", n)
		}
	})

	// I30
	t.Run("I30 hook time limit", func(t *testing.T) {
		e := newEnv(t, pg, 5)
		tx := txpg.NewTransactor(e.pool, txpg.WithHookTimeout(100*time.Millisecond))
		second := false
		start := time.Now()
		err := tx.WithinTx(t.Context(), func(ctx context.Context) error {
			tx.AfterCommit(ctx, func(ctx context.Context) { <-ctx.Done() })
			tx.AfterCommit(ctx, func(context.Context) { second = true })
			return nil
		})
		d := time.Since(start)
		if err != nil || !second || d < 90*time.Millisecond || d > time.Second {
			t.Fatalf("err %v, second %v, duration %v", err, second, d)
		}
		if n := strings.Count(e.logs.String(), "level=ERROR"); n != 1 {
			t.Fatalf("error records = %d: %s", n, e.logs.String())
		}
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("WithHookTimeout(0): want panic")
				}
			}()
			txpg.WithHookTimeout(0)
		}()
	})

	// I31 (run with -race)
	t.Run("I31 concurrent AfterCommit", func(t *testing.T) {
		e := newEnv(t, pg, 5)
		tx := txpg.NewTransactor(e.pool)
		var counter atomic.Int32
		err := tx.WithinTx(t.Context(), func(ctx context.Context) error {
			var wg sync.WaitGroup
			for range 10 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					tx.AfterCommit(ctx, func(context.Context) { counter.Add(1) })
				}()
			}
			wg.Wait()
			return nil
		})
		if err != nil || counter.Load() != 10 {
			t.Fatalf("err %v, counter %d", err, counter.Load())
		}
	})

	// I32
	t.Run("I32 late AfterCommit", func(t *testing.T) {
		for _, fnErr := range []error{nil, errX} {
			e := newEnv(t, pg, 5)
			tx := txpg.NewTransactor(e.pool)
			release := make(chan struct{})
			finished := make(chan struct{})
			var ran atomic.Bool
			_ = tx.WithinTx(t.Context(), func(ctx context.Context) error {
				go func() {
					defer close(finished)
					<-release
					tx.AfterCommit(ctx, func(context.Context) { ran.Store(true) })
				}()
				return fnErr
			})
			close(release)
			<-finished
			if ran.Load() {
				t.Fatalf("fnErr %v: the late hook ran", fnErr)
			}
			if n := strings.Count(e.logs.String(), "level=ERROR"); n != 1 {
				t.Fatalf("fnErr %v: error records = %d: %s", fnErr, n, e.logs.String())
			}
		}
	})

	// I33
	t.Run("I33 foreign transaction", func(t *testing.T) {
		e := newEnv(t, pg, 5)
		tx := txpg.NewTransactor(e.pool)
		foreign, err := e.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer foreign.Rollback(context.Background()) //nolint:errcheck
		ctx := clientpg.ContextWithTx(t.Context(), foreign)
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("AfterCommit: want panic")
				}
			}()
			tx.AfterCommit(ctx, func(context.Context) {})
		}()
		called := false
		err = tx.WithinTx(ctx, func(context.Context) error { called = true; return nil })
		if !errors.Is(err, transaction.ErrForeignTx) || called {
			t.Fatalf("err %v, called %v", err, called)
		}
	})
}
