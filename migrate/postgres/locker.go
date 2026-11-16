package postgres

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/techorionai/constellation-platform-go/migrate"
)

const (
	lockKeyPrefix = "constellation:migrate:"
	closeTimeout  = 5 * time.Second
)

var _ migrate.Locker = (*Locker)(nil)

// Locker is a PostgreSQL session advisory lock.
type Locker struct {
	pool *pgxpool.Pool
	key  int64
}

// NewLocker makes a locker for the migration set name. An empty name gives
// "schema_migrations". Use the same name as MigratorOptions.MigrationsTable.
func NewLocker(pool *pgxpool.Pool, name string) *Locker {
	return &Locker{pool: pool, key: lockKey(name)}
}

// lockKey is the 64-bit FNV-1a hash of "constellation:migrate:" plus name.
func lockKey(name string) int64 {
	if name == "" {
		name = migrate.DefaultName
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(lockKeyPrefix + name))
	return int64(h.Sum64()) //nolint:gosec // the hash value is the key, the sign does not matter
}

// Lock gets one connection from the pool and runs pg_advisory_lock on it. It
// waits until the lock is free or ctx ends. If Lock fails, it closes the
// connection, so that no pool connection keeps a lock that PostgreSQL gave at
// the same time as the cancel.
func (l *Locker) Lock(ctx context.Context) (func(context.Context) error, error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate: get connection for lock: %w", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", l.key); err != nil {
		closeConn(conn)
		return nil, fmt.Errorf("migrate: advisory lock: %w", err)
	}
	var once sync.Once
	unlock := func(ctx context.Context) error {
		var err error
		once.Do(func() {
			var released bool
			if qerr := conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", l.key).Scan(&released); qerr != nil {
				err = fmt.Errorf("migrate: advisory unlock: %w", qerr)
			} else if !released {
				err = errors.New("migrate: advisory unlock: the session did not hold the lock")
			}
			if err != nil {
				// Closing the connection makes PostgreSQL release the lock.
				closeConn(conn)
				return
			}
			conn.Release()
		})
		return err
	}
	return unlock, nil
}

// closeConn removes the connection from the pool and closes it.
func closeConn(conn *pgxpool.Conn) {
	raw := conn.Hijack()
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	_ = raw.Close(ctx)
}
