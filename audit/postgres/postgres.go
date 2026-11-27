package postgres

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/techorionai/constellation-platform-go/audit"
	clientpg "github.com/techorionai/constellation-platform-go/clients/postgres"
	"github.com/techorionai/constellation-platform-go/outbox"
)

const (
	// MigrationsTable is the migrations table of the audit outbox migrations.
	// Use it as the table of the migrator, as the name of the locker, and as
	// Options.Name of migrate.Run.
	MigrationsTable = "platform_audit_outbox_schema_migrations"
	// Table is the name of the audit outbox table.
	Table = "platform_audit_outbox"
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

// Inserter implements audit.Inserter. It inserts in the transaction of the
// context.
type Inserter struct{}

var _ audit.Inserter = (*Inserter)(nil)

// NewInserter makes the inserter of the audit outbox.
func NewInserter() *Inserter { return &Inserter{} }

// InTransaction reports whether ctx has a database transaction.
func (*Inserter) InTransaction(ctx context.Context) bool {
	_, ok := clientpg.TxFromContext(ctx)
	return ok
}

// Insert adds all rows in one batch in the transaction of ctx.
func (*Inserter) Insert(ctx context.Context, rows []outbox.Row) error {
	tx, ok := clientpg.TxFromContext(ctx)
	if !ok {
		return audit.ErrNoTransaction
	}
	batch := &pgx.Batch{}
	for _, r := range rows {
		batch.Queue("INSERT INTO "+Table+" (event_id, type, payload) VALUES ($1, $2, $3)", r.EventID, r.Type, r.Payload)
	}
	br := tx.SendBatch(ctx, batch)
	for range rows {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return fmt.Errorf("audit: insert: %w", err)
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}

// NewWriter makes the audit writer. The pool is not used for writes: the writer
// inserts in the transaction of the context only. A write outside a transaction
// returns audit.ErrNoTransaction.
func NewWriter(_ *pgxpool.Pool, cfg audit.Config) (*audit.EntryWriter, error) {
	return audit.New(NewInserter(), cfg)
}
