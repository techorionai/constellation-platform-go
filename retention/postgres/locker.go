package postgres

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/techorionai/constellation-platform-go/retention"
)

const (
	lockKeyPrefix = "constellation:"
	closeTimeout  = 5 * time.Second
)

// ErrPoolTooSmall means that the pool has a MaxConns below 2. A tick uses the
// lock connection for the full tick and one more connection for each Purge call.
var ErrPoolTooSmall = errors.New("retention: the pool needs MaxConns of 2 or more")

var _ retention.Locker = (*Locker)(nil)

// Locker is a PostgreSQL session advisory lock that does not wait.
type Locker struct {
	pool *pgxpool.Pool
}

// NewLocker makes a locker. It returns [ErrPoolTooSmall] if the pool has a
// MaxConns below 2. It opens no connection.
func NewLocker(pool *pgxpool.Pool) (*Locker, error) {
	if pool == nil {
		return nil, errors.New("retention: pool is required")
	}
	if pool.Config().MaxConns < 2 {
		return nil, ErrPoolTooSmall
	}
	return &Locker{pool: pool}, nil
}

// lockKey is the 64-bit FNV-1a hash of "constellation:" plus name.
func lockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(lockKeyPrefix + name))
	return int64(h.Sum64()) //nolint:gosec // the hash value is the key, the sign does not matter
}

// TryLock gets one connection and runs pg_try_advisory_lock. If another session
// has the lock, it releases the connection and returns acquired false. The
// connection stays with the lock until unlock.
func (l *Locker) TryLock(ctx context.Context, name string) (func(context.Context) error, bool, error) {
	key := lockKey(name)
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("retention: get connection for lock: %w", err)
	}
	var acquired bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		// A canceled query can leave a lock that PostgreSQL gave at the same
		// time. Closing the connection releases it.
		closeConn(conn)
		return nil, false, fmt.Errorf("retention: advisory lock: %w", err)
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}
	var once sync.Once
	unlock := func(ctx context.Context) error {
		var err error
		once.Do(func() {
			var released bool
			if qerr := conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", key).Scan(&released); qerr != nil {
				err = fmt.Errorf("retention: advisory unlock: %w", qerr)
			} else if !released {
				err = errors.New("retention: advisory unlock: the session did not hold the lock")
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
	return unlock, true, nil
}

// closeConn removes the connection from the pool and closes it.
func closeConn(conn *pgxpool.Conn) {
	raw := conn.Hijack()
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	_ = raw.Close(ctx)
}
