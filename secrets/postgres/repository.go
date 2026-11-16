package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	clientpg "github.com/techorionai/constellation-platform-go/clients/postgres"
	"github.com/techorionai/constellation-platform-go/secrets"
)

// MigrationsTable is the migrations table of the secrets migrations. Use it as
// the table of the migrator, as the name of the locker, and as Options.Name of
// migrate.Run.
const MigrationsTable = "platform_secrets_schema_migrations"

//go:embed migrations/*.sql
var files embed.FS

// Migrations returns the embedded migrations with the files at the root. It is
// the result of fs.Sub(files, "migrations").
func Migrations() fs.FS {
	sub, err := fs.Sub(files, "migrations")
	if err != nil {
		// The path is a constant that exists in the embedded files.
		panic(err)
	}
	return sub
}

var _ secrets.Repository = (*Repository)(nil)

// Repository stores the envelopes in the table platform_secrets.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository makes a repository.
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

const columns = "name, format, key_id, wrapped_dek, nonce, ciphertext, created_at, updated_at"

// Upsert inserts a row, or replaces the envelope of an existing row.
func (r *Repository) Upsert(ctx context.Context, p secrets.UpsertParams) error {
	_, err := clientpg.Conn(ctx, r.pool).Exec(ctx, `
INSERT INTO platform_secrets (name, format, key_id, wrapped_dek, nonce, ciphertext)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (name) DO UPDATE SET
    format = EXCLUDED.format, key_id = EXCLUDED.key_id, wrapped_dek = EXCLUDED.wrapped_dek,
    nonce = EXCLUDED.nonce, ciphertext = EXCLUDED.ciphertext, updated_at = now()`,
		p.Name, int16(p.Envelope.Format), p.Envelope.KeyID, p.Envelope.WrappedDEK, p.Envelope.Nonce, p.Envelope.Ciphertext) //nolint:gosec // the format is 1
	if err != nil {
		return fmt.Errorf("secrets: upsert: %w", err)
	}
	return nil
}

// Get returns the record, or secrets.ErrNotFound.
func (r *Repository) Get(ctx context.Context, name string) (secrets.Record, error) {
	rows, err := clientpg.Conn(ctx, r.pool).Query(ctx, `SELECT `+columns+` FROM platform_secrets WHERE name = $1`, name)
	if err != nil {
		return secrets.Record{}, fmt.Errorf("secrets: get: %w", err)
	}
	rec, err := pgx.CollectOneRow(rows, scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return secrets.Record{}, secrets.ErrNotFound
	}
	if err != nil {
		return secrets.Record{}, fmt.Errorf("secrets: get: %w", err)
	}
	return rec, nil
}

// Delete removes the row, or returns secrets.ErrNotFound.
func (r *Repository) Delete(ctx context.Context, name string) error {
	tag, err := clientpg.Conn(ctx, r.pool).Exec(ctx, `DELETE FROM platform_secrets WHERE name = $1`, name)
	if err != nil {
		return fmt.Errorf("secrets: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return secrets.ErrNotFound
	}
	return nil
}

// ListNames returns the names after p.After in ascending order.
func (r *Repository) ListNames(ctx context.Context, p secrets.ListParams) (secrets.ListResult, error) {
	rows, err := clientpg.Conn(ctx, r.pool).Query(ctx,
		`SELECT name FROM platform_secrets WHERE name > $1 ORDER BY name LIMIT $2`, p.After, p.Limit+1)
	if err != nil {
		return secrets.ListResult{}, fmt.Errorf("secrets: list: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return secrets.ListResult{}, fmt.Errorf("secrets: list: %w", err)
	}
	res := secrets.ListResult{Names: names}
	// The query asks for one name more than the limit. An extra name shows that
	// there is a next page.
	if len(names) > p.Limit {
		res.Names = names[:p.Limit]
		res.Next = names[p.Limit-1]
	}
	return res, nil
}

// ListForRewrap returns the records that do not use p.NotKeyID.
func (r *Repository) ListForRewrap(ctx context.Context, p secrets.RewrapListParams) ([]secrets.Record, error) {
	rows, err := clientpg.Conn(ctx, r.pool).Query(ctx, `SELECT `+columns+`
FROM platform_secrets WHERE key_id <> $1 AND name > $2 ORDER BY name LIMIT $3`, p.NotKeyID, p.After, p.Limit)
	if err != nil {
		return nil, fmt.Errorf("secrets: list for rewrap: %w", err)
	}
	recs, err := pgx.CollectRows(rows, scan)
	if err != nil {
		return nil, fmt.Errorf("secrets: list for rewrap: %w", err)
	}
	return recs, nil
}

// UpdateEnvelope replaces the envelope only if the stored envelope is still
// p.Old. Else it returns secrets.ErrConflict.
func (r *Repository) UpdateEnvelope(ctx context.Context, p secrets.UpdateEnvelopeParams) error {
	tag, err := clientpg.Conn(ctx, r.pool).Exec(ctx, `
UPDATE platform_secrets SET format = $4, key_id = $5, wrapped_dek = $6, nonce = $7, ciphertext = $8, updated_at = now()
WHERE name = $1 AND key_id = $2 AND wrapped_dek = $3`,
		p.Name, p.Old.KeyID, p.Old.WrappedDEK,
		int16(p.New.Format), p.New.KeyID, p.New.WrappedDEK, p.New.Nonce, p.New.Ciphertext) //nolint:gosec // the format is 1
	if err != nil {
		return fmt.Errorf("secrets: update envelope: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return secrets.ErrConflict
	}
	return nil
}

func scan(row pgx.CollectableRow) (secrets.Record, error) {
	var (
		rec    secrets.Record
		format int16
	)
	err := row.Scan(&rec.Name, &format, &rec.Envelope.KeyID, &rec.Envelope.WrappedDEK,
		&rec.Envelope.Nonce, &rec.Envelope.Ciphertext, &rec.CreatedAt, &rec.UpdatedAt)
	rec.Envelope.Format = int(format)
	return rec, err
}
