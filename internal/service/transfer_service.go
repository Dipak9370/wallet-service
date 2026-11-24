package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/google/uuid"

	"wallet-service/internal/domain"
	"wallet-service/internal/repository"
)

type Service struct {
	store *repository.Store
}

func New(store *repository.Store) *Service { return &Service{store: store} }

type CreateTransferInput struct {
	IdempotencyKey string
	FromWalletID   string
	ToWalletID     string
	Amount         int64
}

type Result struct {
	Transfer *domain.Transfer
	Replayed bool // true = duplicate request, original result returned
}

func requestHash(from, to string, amount int64) string {
	b, _ := json.Marshal([]any{from, to, amount})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *Service) CreateTransfer(ctx context.Context, in CreateTransferInput) (*Result, error) {
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		return nil, fmt.Errorf("%w: idempotencyKey is required", domain.ErrInvalidRequest)
	}
	// Domain validation (amount, same wallet...) happens BEFORE touching the DB.
	transfer, err := domain.NewTransfer(uuid.NewString(), in.FromWalletID, in.ToWalletID, in.Amount)
	if err != nil {
		return nil, err
	}
	hash := requestHash(in.FromWalletID, in.ToWalletID, in.Amount)

	var result *Result

	// ONE transaction: key + transfer + balances + ledger + status.
	err = s.store.WithTx(ctx, func(repo *repository.Repo) error {
		// 1. Claim the key. The PRIMARY KEY decides the winner even under races.
		claimed, err := repo.ClaimIdempotencyKey(ctx, in.IdempotencyKey, hash, transfer.ID)
		if err != nil {
			return err
		}
		if !claimed {
			// DUPLICATE: return the original result, cause no side effects.
			rec, err := repo.GetIdempotencyRecord(ctx, in.IdempotencyKey)
			if err != nil {
				return err
			}
			if rec.RequestHash != hash {
				return domain.ErrIdempotencyKeyReuse
			}
			original, err := repo.GetTransfer(ctx, rec.TransferID)
			if err != nil {
				return err
			}
			slog.Info("idempotent replay", "key", in.IdempotencyKey, "transfer", original.ID)
			result = &Result{Transfer: original, Replayed: true}
			return nil
		}

		// 2. Save the transfer as PENDING.
		if err := repo.InsertTransfer(ctx, transfer); err != nil {
			return err
		}

		// 3. Lock both wallets in SORTED order -> no deadlock between A->B and B->A.
		ids := []string{in.FromWalletID, in.ToWalletID}
		sort.Strings(ids)
		balances := map[string]int64{}
		for _, id := range ids {
			bal, err := repo.LockWallet(ctx, id)
			if err != nil {
				return err // WalletNotFound -> rollback everything (key not burned)
			}
			balances[id] = bal
		}

		// 4. Decision AFTER the lock, so the balance cannot change under us.
		if balances[in.FromWalletID] < in.Amount {
			if err := transfer.MarkFailed(domain.ReasonInsufficientFunds); err != nil {
				return err
			}
			if err := repo.UpdateStatus(ctx, transfer, domain.StatusPending); err != nil {
				return err
			}
			slog.Info("transfer failed", "id", transfer.ID, "reason", transfer.FailureReason)
			result = &Result{Transfer: transfer}
			return nil // commit: FAILED is a valid, stored outcome
		}

		// 5. Move money + double-entry ledger.
		if err := repo.AdjustBalance(ctx, in.FromWalletID, -in.Amount); err != nil {
			return err
		}
		if err := repo.AdjustBalance(ctx, in.ToWalletID, in.Amount); err != nil {
			return err
		}
		if err := repo.InsertLedgerEntry(ctx, transfer.ID, in.FromWalletID, "DEBIT", in.Amount); err != nil {
			return err
		}
		if err := repo.InsertLedgerEntry(ctx, transfer.ID, in.ToWalletID, "CREDIT", in.Amount); err != nil {
			return err
		}

		// 6. Guarded state transition PENDING -> PROCESSED.
		if err := transfer.MarkProcessed(); err != nil {
			return err
		}
		if err := repo.UpdateStatus(ctx, transfer, domain.StatusPending); err != nil {
			return err
		}
		slog.Info("transfer processed", "id", transfer.ID, "amount", transfer.Amount)
		result = &Result{Transfer: transfer}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Service) GetBalance(ctx context.Context, walletID string) (int64, error) {
	return s.store.GetBalance(ctx, walletID)
}

// CreateWallet is a helper so the API can be demoed. In real life you would fund
// wallets through a deposit transfer from a system wallet.
func (s *Service) CreateWallet(ctx context.Context, id string, opening int64) error {
	if strings.TrimSpace(id) == "" || opening < 0 {
		return fmt.Errorf("%w: id required and openingBalance must be >= 0", domain.ErrInvalidRequest)
	}
	return s.store.CreateWallet(ctx, id, opening)
}
