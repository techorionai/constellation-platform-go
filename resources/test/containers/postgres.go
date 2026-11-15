package containers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/techorionai/constellation-platform-go/config"
)

// DefaultPostgresImage is the default PostgreSQL image.
const DefaultPostgresImage = "postgres:18-alpine"

const (
	adminUser     = "postgres"
	adminPassword = "admin-password"
	adminDatabase = "postgres"
)

type postgresOptions struct {
	image string
}

// PostgresOption changes the PostgreSQL container.
type PostgresOption func(*postgresOptions)

// WithPostgresImage sets the image of the PostgreSQL container.
func WithPostgresImage(image string) PostgresOption {
	return func(o *postgresOptions) { o.image = image }
}

// PostgresInstance is a running PostgreSQL container.
type PostgresInstance struct {
	ctr      *tcpostgres.PostgresContainer
	adminURL *url.URL

	mu      sync.Mutex
	stopped bool
}

// Postgres starts a PostgreSQL container, waits for it, and registers t.Cleanup
// to stop it.
func Postgres(t testing.TB, opts ...PostgresOption) *PostgresInstance {
	t.Helper()
	o := postgresOptions{image: DefaultPostgresImage}
	for _, opt := range opts {
		opt(&o)
	}
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, o.image,
		tcpostgres.WithDatabase(adminDatabase),
		tcpostgres.WithUsername(adminUser),
		tcpostgres.WithPassword(adminPassword),
		tcpostgres.BasicWaitStrategies(),
	)
	if ctr != nil {
		t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })
	}
	if err != nil {
		t.Fatalf("containers: start postgres: %v", err)
	}
	raw, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("containers: postgres connection string: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("containers: parse postgres URL: %v", err)
	}
	return &PostgresInstance{ctr: ctr, adminURL: u}
}

// AdminURL returns the URL of the superuser. It names the database postgres.
func (p *PostgresInstance) AdminURL() config.Secret {
	return config.Secret(p.adminURL.String())
}

// urlFor returns the admin URL with another database name.
func (p *PostgresInstance) urlFor(database string) config.Secret {
	u := *p.adminURL
	u.Path = "/" + database
	return config.Secret(u.String())
}

// NewDatabase makes a database with a random name test_<16 hex>, and registers
// t.Cleanup to drop it. It returns the URL of the superuser for that database
// and the database name.
func (p *PostgresInstance) NewDatabase(t testing.TB) (config.Secret, string) {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("containers: random name: %v", err)
	}
	name := "test_" + hex.EncodeToString(b)
	p.Exec(t, `CREATE DATABASE `+name)
	t.Cleanup(func() {
		p.mu.Lock()
		stopped := p.stopped
		p.mu.Unlock()
		if stopped {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, p.AdminURL().Reveal())
		if err != nil {
			return
		}
		defer conn.Close(ctx) //nolint:errcheck
		_, _ = conn.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})
	return p.urlFor(name), name
}

// Exec runs one SQL statement as the superuser in the database postgres. It
// fails the test on an error.
func (p *PostgresInstance) Exec(t testing.TB, sql string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, p.AdminURL().Reveal())
	if err != nil {
		t.Fatalf("containers: connect as admin: %v", err)
	}
	defer conn.Close(ctx) //nolint:errcheck
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("containers: exec: %v", err)
	}
}

// Host returns the host and port of the server.
func (p *PostgresInstance) Host() string { return p.adminURL.Host }

// Stop stops the container before the cleanup.
func (p *PostgresInstance) Stop(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return nil
	}
	p.stopped = true
	return p.ctr.Stop(ctx, nil)
}
