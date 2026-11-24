package domain

import (
	"errors"
	"fmt"
)

// Sentinel errors. Handlers map these to HTTP codes.
var (
	ErrInvalidRequest         = errors.New("invalid request")
	ErrWalletNotFound         = errors.New("wallet not found")
	ErrWalletExists           = errors.New("wallet already exists")
	ErrIdempotencyKeyReuse    = errors.New("idempotency key reused with a different payload")
	ErrInvalidStateTransition = errors.New("invalid state transition")
)

type Status string

const (
	StatusPending   Status = "PENDING"
	StatusProcessed Status = "PROCESSED"
	StatusFailed    Status = "FAILED"
)

// PENDING -> PROCESSED, PENDING -> FAILED. PROCESSED and FAILED are terminal.
var allowed = map[Status][]Status{
	StatusPending:   {StatusProcessed, StatusFailed},
	StatusProcessed: {},
	StatusFailed:    {},
}

const ReasonInsufficientFunds = "INSUFFICIENT_FUNDS"

type Transfer struct {
	ID            string
	FromWalletID  string
	ToWalletID    string
	Amount        int64 // minor units
	Status        Status
	FailureReason string
}

// NewTransfer validates the business rules and returns a PENDING transfer.
func NewTransfer(id, from, to string, amount int64) (*Transfer, error) {
	if from == "" || to == "" {
		return nil, fmt.Errorf("%w: wallet ids are required", ErrInvalidRequest)
	}
	if from == to {
		return nil, fmt.Errorf("%w: cannot transfer to the same wallet", ErrInvalidRequest)
	}
	if amount <= 0 {
		return nil, fmt.Errorf("%w: amount must be positive", ErrInvalidRequest)
	}
	return &Transfer{
		ID: id, FromWalletID: from, ToWalletID: to,
		Amount: amount, Status: StatusPending,
	}, nil
}

func (t *Transfer) transition(to Status) error {
	for _, s := range allowed[t.Status] {
		if s == to {
			t.Status = to
			return nil
		}
	}
	return fmt.Errorf("%w: %s -> %s", ErrInvalidStateTransition, t.Status, to)
}

func (t *Transfer) MarkProcessed() error { return t.transition(StatusProcessed) }

func (t *Transfer) MarkFailed(reason string) error {
	if err := t.transition(StatusFailed); err != nil {
		return err
	}
	t.FailureReason = reason
	return nil
}
