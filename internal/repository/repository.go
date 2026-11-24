package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"wallet-service/internal/domain"
)

// ---------------------------------------------------------------- Store

// Store owns the connection pool and creates transactions.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// WithTx runs fn inside ONE database transaction.
// fn returns nil -> COMMIT. fn returns error (or panics) -> ROLLBACK.
func (s *Store) WithTx(ctx context.Context, fn func(r *Repo) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	// After a successful Commit, Rollback is a harmless no-op.
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(&Repo{tx: tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GetBalance is a simple read outside a transaction.
func (s *Store) GetBalance(ctx context.Context, walletID string) (int64, error) {
	var bal int64
	err := s.pool.QueryRow(ctx, `SELECT balance FROM wallets WHERE id=$1`, walletID).Scan(&bal)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, domain.ErrWalletNotFound
	}
	return bal, err
}

func (s *Store) CreateWallet(ctx context.Context, id string, opening int64) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO wallets (id, opening_balance, balance) VALUES ($1,$2,$2)`, id, opening)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		return domain.ErrWalletExists
	}
	return err
}

// ---------------------------------------------------------------- Repo

// Repo runs SQL inside one transaction. It contains NO business rules.
type Repo struct {
	tx pgx.Tx
}

type IdempotencyRecord struct {
	RequestHash string
	TransferID  string
}

// ClaimIdempotencyKey returns true if THIS transaction inserted the key (first request).
// If another transaction is still inserting the same key, this call BLOCKS until that
// transaction commits/rolls back, then returns false (or true if it rolled back).
func (r *Repo) ClaimIdempotencyKey(ctx context.Context, key, hash, transferID string) (bool, error) {
	tag, err := r.tx.Exec(ctx, `
		INSERT INTO idempotency_records (idempotency_key, request_hash, transfer_id)
		VALUES ($1,$2,$3)
		ON CONFLICT (idempotency_key) DO NOTHING`, key, hash, transferID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (r *Repo) GetIdempotencyRecord(ctx context.Context, key string) (*IdempotencyRecord, error) {
	var rec IdempotencyRecord
	err := r.tx.QueryRow(ctx,
		`SELECT request_hash, transfer_id FROM idempotency_records WHERE idempotency_key=$1`, key,
	).Scan(&rec.RequestHash, &rec.TransferID)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

func (r *Repo) InsertTransfer(ctx context.Context, t *domain.Transfer) error {
	_, err := r.tx.Exec(ctx, `
		INSERT INTO transfers (id, from_wallet_id, to_wallet_id, amount, status)
		VALUES ($1,$2,$3,$4,$5)`,
		t.ID, t.FromWalletID, t.ToWalletID, t.Amount, string(t.Status))
	return err
}

func (r *Repo) GetTransfer(ctx context.Context, id string) (*domain.Transfer, error) {
	var t domain.Transfer
	var status string
	var reason *string
	err := r.tx.QueryRow(ctx, `
		SELECT id, from_wallet_id, to_wallet_id, amount, status, failure_reason
		FROM transfers WHERE id=$1`, id,
	).Scan(&t.ID, &t.FromWalletID, &t.ToWalletID, &t.Amount, &status, &reason)
	if err != nil {
		return nil, err
	}
	t.Status = domain.Status(status)
	if reason != nil {
		t.FailureReason = *reason
	}
	return &t, nil
}

// UpdateStatus is a GUARDED transition: it only updates if the row is still in
// `expected`. If 0 rows change, someone else already moved it -> error.
func (r *Repo) UpdateStatus(ctx context.Context, t *domain.Transfer, expected domain.Status) error {
	var reason any // nil -> SQL NULL
	if t.FailureReason != "" {
		reason = t.FailureReason
	}
	tag, err := r.tx.Exec(ctx, `
		UPDATE transfers SET status=$1, failure_reason=$2, updated_at=now()
		WHERE id=$3 AND status=$4`,
		string(t.Status), reason, t.ID, string(expected))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrInvalidStateTransition
	}
	return nil
}

// LockWallet takes a row lock (SELECT ... FOR UPDATE) and returns the current balance.
// Other transactions touching this wallet wait until we commit/rollback.
func (r *Repo) LockWallet(ctx context.Context, walletID string) (int64, error) {
	var bal int64
	err := r.tx.QueryRow(ctx,
		`SELECT balance FROM wallets WHERE id=$1 FOR UPDATE`, walletID).Scan(&bal)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, domain.ErrWalletNotFound
	}
	return bal, err
}

func (r *Repo) AdjustBalance(ctx context.Context, walletID string, delta int64) error {
	_, err := r.tx.Exec(ctx, `UPDATE wallets SET balance = balance + $1 WHERE id=$2`, delta, walletID)
	return err
}

func (r *Repo) InsertLedgerEntry(ctx context.Context, transferID, walletID, entryType string, amount int64) error {
	_, err := r.tx.Exec(ctx, `
		INSERT INTO ledger_entries (transfer_id, wallet_id, entry_type, amount)
		VALUES ($1,$2,$3,$4)`, transferID, walletID, entryType, amount)
	return err
}
