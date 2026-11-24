package repository

import (
	"context"
	_ "embed"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// Migrate creates the tables (idempotent: uses IF NOT EXISTS).
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, schemaSQL) // no args -> simple protocol -> multi-statement OK
	return err
}
