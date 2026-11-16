package postgres

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/techorionai/constellation-platform-go/retention"
)

var (
	identPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

	_ retention.Purger = (*TablePurger)(nil)
)

// TableSpec names the table that a [TablePurger] cleans. Each name matches
// ^[a-z_][a-z0-9_]{0,62}$.
type TableSpec struct {
	// Schema is the schema of the table. The default is "public".
	Schema string
	// Table is the table name.
	Table string
	// TimeColumn is the column with the time. It has the type timestamptz. A row
	// is expired when this time is before the expiry time. Add an index on it.
	TimeColumn string
	// KeyColumn is the primary key, or a part of the primary key of a
	// partitioned table. The default is "id".
	KeyColumn string
}

// TablePurger deletes the expired rows of one table.
type TablePurger struct {
	pool *pgxpool.Pool
	sql  string
}

// NewTablePurger checks the names in spec and makes a purger. It quotes the
// names. It runs no SQL.
func NewTablePurger(pool *pgxpool.Pool, spec TableSpec) (*TablePurger, error) {
	if spec.Schema == "" {
		spec.Schema = "public"
	}
	if spec.KeyColumn == "" {
		spec.KeyColumn = "id"
	}
	for field, v := range map[string]string{
		"schema": spec.Schema, "table": spec.Table, "time column": spec.TimeColumn, "key column": spec.KeyColumn,
	} {
		if !identPattern.MatchString(v) {
			return nil, fmt.Errorf("retention: the %s name must match %s", field, identPattern)
		}
	}
	if pool == nil {
		return nil, errors.New("retention: pool is required")
	}
	t := pgx.Identifier{spec.Schema, spec.Table}.Sanitize()
	col := pgx.Identifier{spec.TimeColumn}.Sanitize()
	key := pgx.Identifier{spec.KeyColumn}.Sanitize()
	// The key column makes the statement work on a partitioned table. A ctid
	// is unique in one partition only. The outer time condition keeps the rows
	// that are not expired, also when the key is unique only with the partition
	// key.
	sql := fmt.Sprintf(
		"DELETE FROM %[1]s WHERE %[3]s IN (SELECT %[3]s FROM %[1]s WHERE %[2]s < $1 ORDER BY %[2]s LIMIT $2) AND %[2]s < $1",
		t, col, key)
	return &TablePurger{pool: pool, sql: sql}, nil
}

// Purge deletes at most params.Limit rows that are older than params.Before.
func (p *TablePurger) Purge(ctx context.Context, params retention.PurgeParams) (int, error) {
	if params.Limit < 1 {
		return 0, errors.New("retention: the limit must be 1 or more")
	}
	tag, err := p.pool.Exec(ctx, p.sql, params.Before, params.Limit)
	if err != nil {
		return 0, fmt.Errorf("retention: purge: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
