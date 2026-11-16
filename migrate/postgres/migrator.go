package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"

	gomigrate "github.com/golang-migrate/migrate/v4"
	pgxdriver "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/techorionai/constellation-platform-go/migrate"
)

// ErrPoolTooSmall means that the pool has a MaxConns below 2. migrate.Run uses
// two connections at the same time: one for the lock, and one for
// golang-migrate.
var ErrPoolTooSmall = errors.New("migrate: the pool needs MaxConns of 2 or more")

var tablePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

var _ migrate.Migrator = (*Migrator)(nil)

// MigratorOptions are the settings for [NewMigrator].
type MigratorOptions struct {
	// MigrationsTable is the table that stores the version. The default is
	// "schema_migrations". It must match ^[a-z_][a-z0-9_]{0,62}$.
	MigrationsTable string
}

// Migrator applies the up migrations with golang-migrate.
type Migrator struct {
	pool   *pgxpool.Pool
	source fs.FS
	table  string
}

// NewMigrator checks its arguments. It does no database work and gets no
// connection. The migration files must be at the root of source. A service
// that embeds migrations/*.sql passes fs.Sub(files, "migrations").
func NewMigrator(pool *pgxpool.Pool, source fs.FS, opts MigratorOptions) (*Migrator, error) {
	if pool == nil {
		return nil, errors.New("migrate: pool is required")
	}
	if source == nil {
		return nil, errors.New("migrate: source is required")
	}
	if pool.Config().MaxConns < 2 {
		return nil, ErrPoolTooSmall
	}
	table := opts.MigrationsTable
	if table == "" {
		table = migrate.DefaultName
	}
	if !tablePattern.MatchString(table) {
		return nil, fmt.Errorf("migrate: migrations table must match %s", tablePattern)
	}
	src, err := iofs.New(source, ".")
	if err != nil {
		return nil, fmt.Errorf("migrate: read migration files: %w", err)
	}
	defer src.Close() //nolint:errcheck
	if _, err := src.First(); err != nil {
		return nil, errors.New("migrate: the source has no migration file")
	}
	return &Migrator{pool: pool, source: source, table: table}, nil
}

// Up applies all pending up migrations on one pool connection. Call it only
// inside the lock, as migrate.Run does. If the database version is higher than
// the newest source version, Up applies nothing and returns Result.Ahead. If ctx
// ends, Up completes the current migration and returns ctx.Err().
func (m *Migrator) Up(ctx context.Context) (migrate.Result, error) {
	if err := ctx.Err(); err != nil {
		return migrate.Result{}, err
	}
	src, err := iofs.New(m.source, ".")
	if err != nil {
		return migrate.Result{}, fmt.Errorf("migrate: read migration files: %w", err)
	}
	versions, err := sourceVersions(src)
	if err != nil {
		_ = src.Close()
		return migrate.Result{}, err
	}

	db := stdlib.OpenDBFromPool(m.pool)
	drv, err := pgxdriver.WithInstance(db, &pgxdriver.Config{MigrationsTable: m.table})
	if err != nil {
		_ = src.Close()
		_ = db.Close()
		return migrate.Result{}, fmt.Errorf("migrate: open driver: %w", err)
	}
	mg, err := gomigrate.NewWithInstance("iofs", src, "pgx5", drv)
	if err != nil {
		_ = src.Close()
		_ = drv.Close()
		_ = db.Close()
		return migrate.Result{}, fmt.Errorf("migrate: open migrator: %w", err)
	}
	// Close the source, the driver, the connection, and the sql.DB. The pool
	// stays open.
	defer closeAll(mg, db)

	from, dirty, err := mg.Version()
	if err != nil && !errors.Is(err, gomigrate.ErrNilVersion) {
		return migrate.Result{}, fmt.Errorf("migrate: read version: %w", err)
	}
	if dirty {
		return migrate.Result{FromVersion: from}, dirtyError(from)
	}
	newest := versions[len(versions)-1]
	if from > newest {
		return migrate.Result{FromVersion: from, ToVersion: from, Ahead: true}, nil
	}

	// Apply the pending migrations one at a time and check ctx between them. A
	// stop signal lets the current migration complete. The GracefulStop channel
	// of golang-migrate is not used, because its stop check has a data race
	// between two goroutines of the library (found with go test -race).
	for _, v := range versions {
		if v <= from {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		if upErr := mg.Steps(1); upErr != nil && !errors.Is(upErr, gomigrate.ErrNoChange) {
			var dirtyErr gomigrate.ErrDirty
			if errors.As(upErr, &dirtyErr) {
				return migrate.Result{FromVersion: from}, dirtyError(uint(dirtyErr.Version)) //nolint:gosec // the version is not negative
			}
			return migrate.Result{FromVersion: from}, fmt.Errorf("migrate: up: %w", upErr)
		}
	}

	to, dirty, err := mg.Version()
	if err != nil && !errors.Is(err, gomigrate.ErrNilVersion) {
		return migrate.Result{FromVersion: from}, fmt.Errorf("migrate: read version: %w", err)
	}
	res := migrate.Result{FromVersion: from, ToVersion: to}
	for _, v := range versions {
		if v > from && v <= to {
			res.Applied++
		}
	}
	if dirty {
		return res, dirtyError(to)
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	return res, nil
}

func dirtyError(version uint) error {
	return fmt.Errorf("%w: version %d", migrate.ErrDirty, version)
}

type versionSource interface {
	First() (uint, error)
	Next(version uint) (uint, error)
}

// sourceVersions returns the sorted versions of the source. It returns an error
// if the source has no version.
func sourceVersions(src versionSource) ([]uint, error) {
	v, err := src.First()
	if err != nil {
		return nil, errors.New("migrate: the source has no migration file")
	}
	versions := []uint{v}
	for {
		v, err = src.Next(v)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("migrate: read migration files: %w", err)
		}
		versions = append(versions, v)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	return versions, nil
}

func closeAll(mg *gomigrate.Migrate, db *sql.DB) {
	_, _ = mg.Close()
	_ = db.Close()
}
