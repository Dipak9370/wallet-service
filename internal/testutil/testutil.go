// Package testutil has helpers for integration tests.
// Every test gets its OWN Postgres schema, so tests are isolated and can run in parallel.
package testutil

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"wallet-service/internal/config"
	"wallet-service/internal/repository"
)

func dsn() string {
	config.LoadDotEnv() // reads .env from project root
	return config.Getenv("TEST_DATABASE_URL",
		"postgres://postgres:postgres@localhost:5432/wallet?sslmode=disable")
}

// NewDB creates an isolated schema, runs the migration in it, and cleans up after the test.
func NewDB(t *testing.T) (*pgxpool.Pool, *repository.Store) {
	t.Helper()
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dsn())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Fatalf("cannot reach Postgres (run `docker compose up -d`): %v", err)
	}

	schema := "t_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(dsn())
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.MaxConns = 30
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := repository.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	return pool, repository.NewStore(pool)
}

func SeedWallet(t *testing.T, pool *pgxpool.Pool, id string, balance int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO wallets (id, opening_balance, balance) VALUES ($1,$2,$2)`, id, balance)
	if err != nil {
		t.Fatalf("seed wallet %s: %v", id, err)
	}
}

func BalanceOf(t *testing.T, pool *pgxpool.Pool, id string) int64 {
	t.Helper()
	var b int64
	if err := pool.QueryRow(context.Background(),
		`SELECT balance FROM wallets WHERE id=$1`, id).Scan(&b); err != nil {
		t.Fatalf("balance of %s: %v", id, err)
	}
	return b
}

// Count runs a COUNT(*) query and returns the number.
func Count(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// AssertLedgerConsistent checks the global invariants that must hold after ANY operations.
func AssertLedgerConsistent(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	// 1. Total debits == total credits (the ledger balances).
	var debits, credits int64
	err := pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount) FILTER (WHERE entry_type='DEBIT'),0)::bigint,
		       COALESCE(SUM(amount) FILTER (WHERE entry_type='CREDIT'),0)::bigint
		FROM ledger_entries`).Scan(&debits, &credits)
	if err != nil {
		t.Fatalf("sum ledger: %v", err)
	}
	if debits != credits {
		t.Errorf("ledger unbalanced: debits=%d credits=%d", debits, credits)
	}

	// 2. PROCESSED transfer => exactly 2 entries. FAILED/PENDING => 0 entries.
	rows, err := pool.Query(ctx, `
		SELECT t.id, t.status, COUNT(l.id)
		FROM transfers t LEFT JOIN ledger_entries l ON l.transfer_id = t.id
		GROUP BY t.id, t.status`)
	if err != nil {
		t.Fatalf("entries per transfer: %v", err)
	}
	for rows.Next() {
		var id, status string
		var n int64
		if err := rows.Scan(&id, &status, &n); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		want := int64(0)
		if status == "PROCESSED" {
			want = 2
		}
		if n != want {
			t.Errorf("transfer %s status=%s has %d ledger entries, want %d", id, status, n, want)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	// 3. Stored balance == opening + credits - debits, for every wallet.
	rows, err = pool.Query(ctx, `
		SELECT w.id, w.balance,
		       (w.opening_balance + COALESCE(SUM(
		           CASE l.entry_type WHEN 'CREDIT' THEN l.amount ELSE -l.amount END),0))::bigint
		FROM wallets w LEFT JOIN ledger_entries l ON l.wallet_id = w.id
		GROUP BY w.id`)
	if err != nil {
		t.Fatalf("balance check: %v", err)
	}
	for rows.Next() {
		var id string
		var stored, derived int64
		if err := rows.Scan(&id, &stored, &derived); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		if stored != derived {
			t.Errorf("wallet %s stored balance=%d but ledger says %d", id, stored, derived)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
}
