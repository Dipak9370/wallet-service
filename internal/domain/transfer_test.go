package domain_test

import (
	"errors"
	"testing"

	"wallet-service/internal/domain"
)

func TestNewTransferValidation(t *testing.T) {
	cases := []struct {
		name     string
		from, to string
		amount   int64
	}{
		{"zero amount", "a", "b", 0},
		{"negative amount", "a", "b", -5},
		{"same wallet", "a", "a", 10},
		{"empty from", "", "b", 10},
		{"empty to", "a", "", 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := domain.NewTransfer("id", c.from, c.to, c.amount)
			if !errors.Is(err, domain.ErrInvalidRequest) {
				t.Fatalf("want ErrInvalidRequest, got %v", err)
			}
		})
	}
}

func TestPendingCanBecomeProcessed(t *testing.T) {
	tr, _ := domain.NewTransfer("id", "a", "b", 10)
	if err := tr.MarkProcessed(); err != nil || tr.Status != domain.StatusProcessed {
		t.Fatalf("unexpected: status=%s err=%v", tr.Status, err)
	}
}

func TestPendingCanBecomeFailed(t *testing.T) {
	tr, _ := domain.NewTransfer("id", "a", "b", 10)
	if err := tr.MarkFailed("x"); err != nil || tr.Status != domain.StatusFailed || tr.FailureReason != "x" {
		t.Fatalf("unexpected: %+v err=%v", tr, err)
	}
}

func TestTerminalStatesCannotChange(t *testing.T) {
	tr, _ := domain.NewTransfer("id", "a", "b", 10)
	_ = tr.MarkProcessed()
	if err := tr.MarkFailed("x"); !errors.Is(err, domain.ErrInvalidStateTransition) {
		t.Fatalf("PROCESSED->FAILED should be rejected, got %v", err)
	}
	if err := tr.MarkProcessed(); !errors.Is(err, domain.ErrInvalidStateTransition) {
		t.Fatalf("PROCESSED->PROCESSED should be rejected, got %v", err)
	}

	tr2, _ := domain.NewTransfer("id2", "a", "b", 10)
	_ = tr2.MarkFailed("x")
	if err := tr2.MarkProcessed(); !errors.Is(err, domain.ErrInvalidStateTransition) {
		t.Fatalf("FAILED->PROCESSED should be rejected, got %v", err)
	}
}
