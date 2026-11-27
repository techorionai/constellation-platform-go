package postgres

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	clientpg "github.com/techorionai/constellation-platform-go/clients/postgres"
	"github.com/techorionai/constellation-platform-go/outbox"
)

const (
	// MigrationsTable is the migrations table of the outbox migrations. Use it as
	// the table of the migrator, as the name of the locker, and as Options.Name
	// of migrate.Run.
	MigrationsTable = "platform_outbox_schema_migrations"
	// Table is the name of the outbox table.
	Table = "platform_outbox"
)

//go:embed migrations/*.sql
var files embed.FS

// Migrations returns the embedded migrations with the files at the root.
func Migrations() fs.FS {
	sub, err := fs.Sub(files, "migrations")
	if err != nil {
		// The path is a constant that exists in the embedded files.
		panic(err)
	}
	return sub
}

// inserter implements outbox.Inserter.
type inserter struct{}

var _ outbox.Inserter = inserter{}

func (inserter) InTransaction(ctx context.Context) bool {
	_, ok := clientpg.TxFromContext(ctx)
	return ok
}

func (inserter) Insert(ctx context.Context, rows []outbox.Row) error {
	tx, ok := clientpg.TxFromContext(ctx)
	if !ok {
		return outbox.ErrNoTransaction
	}
	batch := &pgx.Batch{}
	for _, r := range rows {
		batch.Queue("INSERT INTO "+Table+" (event_id, type, payload) VALUES ($1, $2, $3)", r.EventID, r.Type, r.Payload)
	}
	br := tx.SendBatch(ctx, batch)
	for range rows {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return fmt.Errorf("outbox: insert: %w", err)
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("outbox: insert: %w", err)
	}
	return nil
}

// NewWriter makes the outbox writer. The pool is not used for writes: the
// writer inserts in the transaction of the context only. A write outside a
// transaction returns outbox.ErrNoTransaction.
func NewWriter(_ *pgxpool.Pool, cfg outbox.Config) (*outbox.EventWriter, error) {
	return outbox.New(cfg, inserter{})
}

var identPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Store implements outbox.Store.
type Store struct {
	pool  *pgxpool.Pool
	table string
}

var _ outbox.Store = (*Store)(nil)

// NewStore makes the store of the relay for the table [Table].
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool, table: Table} }

// NewStoreFor makes the store of the relay for another table with the columns
// of [Table], for example the audit outbox. The name must match
// ^[a-z_][a-z0-9_]{0,62}$. The function quotes it. An invalid name gives an
// error that wraps outbox.ErrInvalidConfig.
func NewStoreFor(pool *pgxpool.Pool, table string) (*Store, error) {
	if !identPattern.MatchString(table) {
		return nil, fmt.Errorf("%w: the table name must match %s", outbox.ErrInvalidConfig, identPattern)
	}
	return &Store{pool: pool, table: pgx.Identifier{table}.Sanitize()}, nil
}

// Process locks unpublished rows, calls fn, and marks the rows as published in
// the same transaction. See outbox.Store.
//
// The row locks stay while fn runs. A failure after fn returns nil and before
// the commit, for example a lost connection, rolls back the mark. The next call
// then publishes the same events again, so a consumer can receive a duplicate.
func (s *Store) Process(ctx context.Context, limit, maxBytes int, fn func(ctx context.Context, events []outbox.Record) error) (int, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("outbox: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	rows, err := tx.Query(ctx,
		"SELECT id, event_id::text, payload FROM "+s.table+
			" WHERE published_at IS NULL ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED", limit)
	if err != nil {
		return 0, false, fmt.Errorf("outbox: select: %w", err)
	}
	var (
		recs     []outbox.Record
		selected int
		size     int
		byBytes  bool
	)
	for rows.Next() {
		var r outbox.Record
		if err := rows.Scan(&r.ID, &r.EventID, &r.Payload); err != nil {
			rows.Close()
			return 0, false, fmt.Errorf("outbox: scan: %w", err)
		}
		selected++
		if byBytes {
			continue
		}
		if len(recs) > 0 && size+len(r.Payload) > maxBytes {
			byBytes = true
			continue
		}
		size += len(r.Payload)
		recs = append(recs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("outbox: select: %w", err)
	}
	if len(recs) == 0 {
		return 0, false, nil
	}
	if err := fn(ctx, recs); err != nil {
		return 0, false, err
	}
	ids := make([]int64, len(recs))
	for i := range recs {
		ids[i] = recs[i].ID
	}
	if _, err := tx.Exec(ctx, "UPDATE "+s.table+" SET published_at = now() WHERE id = ANY($1)", ids); err != nil {
		return 0, false, fmt.Errorf("outbox: mark published: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("outbox: commit: %w", err)
	}
	return len(recs), selected >= limit || byBytes, nil
}

// Lag returns the age of the oldest unpublished row, or 0.
func (s *Store) Lag(ctx context.Context) (time.Duration, error) {
	var seconds float64
	err := s.pool.QueryRow(ctx,
		"SELECT COALESCE(EXTRACT(EPOCH FROM now() - min(created_at)), 0)::float8 FROM "+s.table+
			" WHERE published_at IS NULL").Scan(&seconds)
	if err != nil {
		return 0, fmt.Errorf("outbox: lag: %w", err)
	}
	return max(time.Duration(seconds*float64(time.Second)), 0), nil
}
