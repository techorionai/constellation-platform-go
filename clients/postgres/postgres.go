package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/techorionai/constellation-platform-go/config"
)

// Errors returned by [Config.ConnString] and [New]. No error contains the URL
// or the password.
var (
	// ErrInvalidURL means that the URL does not start with postgres:// or
	// postgresql://, or that it cannot be parsed.
	ErrInvalidURL = errors.New("postgres: URL must be a postgres:// or postgresql:// URL")
	// ErrInvalidDatabaseName means that the database name is empty or does not
	// match ^[a-z_][a-z0-9_]{0,62}$.
	ErrInvalidDatabaseName = errors.New("postgres: invalid database name")
	// ErrSystemDatabase means that the database name is postgres, template0, or
	// template1.
	ErrSystemDatabase = errors.New("postgres: system database is not allowed")
	// ErrDatabaseMismatch means that the database in the URL, or the database
	// of the connection, is not the configured database.
	ErrDatabaseMismatch = errors.New("postgres: database does not match the configured database")
)

const (
	defaultMaxConns       = 10
	defaultConnectTimeout = 5 * time.Second
)

// forbiddenQueryKeys are the query keys of the URL that change the target of the
// connection. pgx lets them override the parts of the URL, so a consumer of
// [Config.ConnString] could connect to a different database or server.
var forbiddenQueryKeys = map[string]struct{}{
	"dbname": {}, "database": {}, "host": {}, "hostaddr": {},
	"port": {}, "service": {}, "servicefile": {},
}

var databaseNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Config is the configuration of the PostgreSQL client. A service nests it with
// envPrefix:"DATABASE__".
type Config struct {
	// URL gives the host, port, user, password, and TLS settings. The database
	// name in the URL is optional. If it is present, it must be Database.
	URL config.Secret `env:"URL,required" envFile:"true"`
	// Database is the name of the database of the service. It is required.
	Database string `env:"NAME,required"`
	// MaxConns is the maximum size of the pool. The default is 10.
	MaxConns int32 `env:"MAX_CONNS" envDefault:"10"`
	// MinConns is the minimum size of the pool. The default is 0.
	MinConns int32 `env:"MIN_CONNS" envDefault:"0"`
	// ConnectTimeout is the time limit for one new connection. The default is 5
	// seconds.
	ConnectTimeout time.Duration `env:"CONNECT_TIMEOUT" envDefault:"5s"`
}

// parse checks the URL and the database name. It returns the parsed URL.
func (c Config) parse() (*url.URL, error) {
	raw := c.URL.Reveal()
	if !strings.HasPrefix(raw, "postgres://") && !strings.HasPrefix(raw, "postgresql://") {
		return nil, ErrInvalidURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, ErrInvalidURL
	}
	if db := strings.TrimPrefix(u.Path, "/"); db != "" && db != c.Database {
		return nil, ErrDatabaseMismatch
	}
	for key := range u.Query() {
		if _, bad := forbiddenQueryKeys[strings.ToLower(key)]; bad {
			return nil, fmt.Errorf("%w: the query key %q is not allowed", ErrInvalidURL, key)
		}
	}
	switch c.Database {
	case "postgres", "template0", "template1":
		return nil, ErrSystemDatabase
	}
	if !databaseNamePattern.MatchString(c.Database) {
		return nil, ErrInvalidDatabaseName
	}
	return u, nil
}

// ConnString returns the URL with the database set to Database. Other packages
// that need a connection string must use it, and not URL directly.
func (c Config) ConnString() (config.Secret, error) {
	u, err := c.parse()
	if err != nil {
		return "", err
	}
	u.Path = "/" + c.Database
	u.RawPath = ""
	return config.Secret(u.String()), nil
}

// Client is a PostgreSQL connection pool with tracing and pool metrics.
type Client struct {
	// Pool is the connection pool.
	Pool *pgxpool.Pool

	reg metric.Registration
}

// New creates the pool, checks the database with SELECT current_database(), and
// registers the pool metrics.
func New(ctx context.Context, cfg Config) (*Client, error) {
	connString, err := cfg.ConnString()
	if err != nil {
		return nil, err
	}
	warnWeakTLS(connString)
	poolCfg, err := pgxpool.ParseConfig(connString.Reveal())
	if err != nil {
		return nil, ErrInvalidURL
	}
	poolCfg.ConnConfig.Database = cfg.Database
	poolCfg.ConnConfig.Tracer = otelpgx.NewTracer()
	poolCfg.MaxConns = cfg.MaxConns
	if poolCfg.MaxConns == 0 {
		poolCfg.MaxConns = defaultMaxConns
	}
	poolCfg.MinConns = cfg.MinConns
	if poolCfg.MinConns < 0 || poolCfg.MinConns > poolCfg.MaxConns {
		return nil, errors.New("postgres: MinConns must be between 0 and MaxConns")
	}
	poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	if poolCfg.ConnConfig.ConnectTimeout == 0 {
		poolCfg.ConnConfig.ConnectTimeout = defaultConnectTimeout
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot create pool: %w", redact(err, poolCfg.ConnConfig.Password))
	}
	var current string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&current); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: cannot connect to database %q: %w", cfg.Database, redact(err, poolCfg.ConnConfig.Password))
	}
	if current != cfg.Database {
		pool.Close()
		return nil, ErrDatabaseMismatch
	}
	reg, err := registerMetrics(pool, cfg.Database, poolCfg.MinConns)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: cannot register metrics: %w", err)
	}
	return &Client{Pool: pool, reg: reg}, nil
}

// warnWeakTLS writes a warn log record if the URL does not force TLS. The sslmode
// values disable, allow, and prefer, and a URL without sslmode, can send the
// password and the data in plain text. The record has the database name only.
func warnWeakTLS(connString config.Secret) {
	u, err := url.Parse(connString.Reveal())
	if err != nil {
		return
	}
	switch u.Query().Get("sslmode") {
	case "", "disable", "allow", "prefer":
		slog.Warn("postgres: the URL does not require TLS, so the connection can use plain text",
			slog.String("database", strings.TrimPrefix(u.Path, "/")))
	}
}

// Close stops the pool metrics, then closes the pool.
func (c *Client) Close() {
	if c.reg != nil {
		_ = c.reg.Unregister()
	}
	c.Pool.Close()
}

// redact returns an error with the same text as err, but without the password.
func redact(err error, password string) error {
	if password == "" || !strings.Contains(err.Error(), password) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), password, "[redacted]"))
}

func registerMetrics(pool *pgxpool.Pool, database string, minConns int32) (metric.Registration, error) {
	m := otel.GetMeterProvider().Meter("github.com/techorionai/constellation-platform-go/clients/postgres")
	const unitConn = "{connection}"

	count, err := m.Int64ObservableUpDownCounter("db.client.connection.count", metric.WithUnit(unitConn),
		metric.WithDescription("Number of connections in the pool, by state."))
	if err != nil {
		return nil, err
	}
	maxC, err := m.Int64ObservableUpDownCounter("db.client.connection.max", metric.WithUnit(unitConn),
		metric.WithDescription("Maximum number of connections in the pool."))
	if err != nil {
		return nil, err
	}
	idleMin, err := m.Int64ObservableUpDownCounter("db.client.connection.idle.min", metric.WithUnit(unitConn),
		metric.WithDescription("Minimum number of connections in the pool."))
	if err != nil {
		return nil, err
	}
	constructing, err := m.Int64ObservableUpDownCounter("pgxpool.connection.constructing", metric.WithUnit(unitConn),
		metric.WithDescription("Number of connections that the pool is making now."))
	if err != nil {
		return nil, err
	}
	acquires, err := m.Int64ObservableCounter("pgxpool.acquires", metric.WithUnit("{acquire}"),
		metric.WithDescription("Number of acquires, by result."))
	if err != nil {
		return nil, err
	}
	acquireTime, err := m.Float64ObservableCounter("pgxpool.acquire.time", metric.WithUnit("s"),
		metric.WithDescription("Total time that acquires took."))
	if err != nil {
		return nil, err
	}
	created, err := m.Int64ObservableCounter("pgxpool.connections.created", metric.WithUnit(unitConn),
		metric.WithDescription("Number of connections that the pool made."))
	if err != nil {
		return nil, err
	}
	closed, err := m.Int64ObservableCounter("pgxpool.connections.closed", metric.WithUnit(unitConn),
		metric.WithDescription("Number of connections that the pool closed, by reason."))
	if err != nil {
		return nil, err
	}

	return m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		s := pool.Stat()
		o.ObserveInt64(count, int64(s.IdleConns()), withPool(database, attribute.String("db.client.connection.state", "idle")))
		o.ObserveInt64(count, int64(s.AcquiredConns()), withPool(database, attribute.String("db.client.connection.state", "used")))
		o.ObserveInt64(maxC, int64(s.MaxConns()), withPool(database))
		o.ObserveInt64(idleMin, int64(minConns), withPool(database))
		o.ObserveInt64(constructing, int64(s.ConstructingConns()), withPool(database))
		o.ObserveInt64(acquires, s.AcquireCount()-s.EmptyAcquireCount(), withPool(database, attribute.String("pgxpool.acquire.result", "idle")))
		o.ObserveInt64(acquires, s.EmptyAcquireCount(), withPool(database, attribute.String("pgxpool.acquire.result", "empty")))
		o.ObserveInt64(acquires, s.CanceledAcquireCount(), withPool(database, attribute.String("pgxpool.acquire.result", "canceled")))
		o.ObserveFloat64(acquireTime, s.AcquireDuration().Seconds(), withPool(database))
		o.ObserveInt64(created, s.NewConnsCount(), withPool(database))
		o.ObserveInt64(closed, s.MaxLifetimeDestroyCount(), withPool(database, attribute.String("pgxpool.close.reason", "max_lifetime")))
		o.ObserveInt64(closed, s.MaxIdleDestroyCount(), withPool(database, attribute.String("pgxpool.close.reason", "max_idle")))
		return nil
	}, count, maxC, idleMin, constructing, acquires, acquireTime, created, closed)
}
