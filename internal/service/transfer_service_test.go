package service_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"wallet-service/internal/domain"
	"wallet-service/internal/service"
	"wallet-service/internal/testutil"
)

func setup(t *testing.T) (*service.Service, *pgxpool.Pool) {
	t.Helper()
	pool, store := testutil.NewDB(t)
	return service.New(store), pool
}

func in(key, from, to string, amount int64) service.CreateTransferInput {
	return service.CreateTransferInput{IdempotencyKey: key, FromWalletID: from, ToWalletID: to, Amount: amount}
}

// runParallel starts n goroutines at the same moment and returns all errors.
func runParallel(n int, fn func(i int) error) []error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if err := fn(i); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()
	return errs
}

// ------------------------------------------------------------ basic behaviour

func TestSuccessfulTransferMovesMoneyAndWritesTwoEntries(t *testing.T) {
	svc, pool := setup(t)
	testutil.SeedWallet(t, pool, "w1", 500)
	testutil.SeedWallet(t, pool, "w2", 0)

	res, err := svc.CreateTransfer(context.Background(), in("k1", "w1", "w2", 100))
	if err != nil {
		t.Fatal(err)
	}
	if res.Transfer.Status != domain.StatusProcessed || res.Replayed {
		t.Fatalf("unexpected result: %+v", res)
	}
	if b := testutil.BalanceOf(t, pool, "w1"); b != 400 {
		t.Errorf("w1 = %d, want 400", b)
	}
	if b := testutil.BalanceOf(t, pool, "w2"); b != 100 {
		t.Errorf("w2 = %d, want 100", b)
	}
	if n := testutil.Count(t, pool, `SELECT COUNT(*) FROM ledger_entries WHERE wallet_id='w1' AND entry_type='DEBIT' AND amount=100`); n != 1 {
		t.Errorf("want 1 DEBIT on w1, got %d", n)
	}
	if n := testutil.Count(t, pool, `SELECT COUNT(*) FROM ledger_entries WHERE wallet_id='w2' AND entry_type='CREDIT' AND amount=100`); n != 1 {
		t.Errorf("want 1 CREDIT on w2, got %d", n)
	}
	testutil.AssertLedgerConsistent(t, pool)
}

func TestInsufficientFundsMarksFailedWithoutLedgerEntries(t *testing.T) {
	svc, pool := setup(t)
	testutil.SeedWallet(t, pool, "w1", 50)
	testutil.SeedWallet(t, pool, "w2", 0)

	res, err := svc.CreateTransfer(context.Background(), in("k1", "w1", "w2", 100))
	if err != nil {
		t.Fatal(err)
	}
	if res.Transfer.Status != domain.StatusFailed || res.Transfer.FailureReason != domain.ReasonInsufficientFunds {
		t.Fatalf("unexpected: %+v", res.Transfer)
	}
	if testutil.BalanceOf(t, pool, "w1") != 50 || testutil.BalanceOf(t, pool, "w2") != 0 {
		t.Error("balances must be unchanged")
	}
	if n := testutil.Count(t, pool, `SELECT COUNT(*) FROM ledger_entries`); n != 0 {
		t.Errorf("want 0 ledger entries, got %d", n)
	}
	testutil.AssertLedgerConsistent(t, pool)
}

func TestUnknownWalletRollsBackEverythingAndKeyIsNotBurned(t *testing.T) {
	svc, pool := setup(t)
	testutil.SeedWallet(t, pool, "w1", 100)

	_, err := svc.CreateTransfer(context.Background(), in("k1", "w1", "ghost", 10))
	if !errors.Is(err, domain.ErrWalletNotFound) {
		t.Fatalf("want ErrWalletNotFound, got %v", err)
	}
	if n := testutil.Count(t, pool, `SELECT COUNT(*) FROM transfers`); n != 0 {
		t.Errorf("transfers should be rolled back, got %d", n)
	}
	if n := testutil.Count(t, pool, `SELECT COUNT(*) FROM idempotency_records`); n != 0 {
		t.Errorf("idempotency key must NOT be stored after rollback, got %d", n)
	}

	// Create the missing wallet; the SAME key must work now (retry-safe).
	testutil.SeedWallet(t, pool, "ghost", 0)
	res, err := svc.CreateTransfer(context.Background(), in("k1", "w1", "ghost", 10))
	if err != nil || res.Transfer.Status != domain.StatusProcessed {
		t.Fatalf("retry with same key should succeed: res=%+v err=%v", res, err)
	}
}

func TestInvalidInputIsRejected(t *testing.T) {
	svc, pool := setup(t)
	testutil.SeedWallet(t, pool, "w1", 100)
	testutil.SeedWallet(t, pool, "w2", 0)

	cases := map[string]service.CreateTransferInput{
		"zero amount":     in("k", "w1", "w2", 0),
		"negative amount": in("k", "w1", "w2", -5),
		"same wallet":     in("k", "w1", "w1", 10),
		"empty key":       in("", "w1", "w2", 10),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := svc.CreateTransfer(context.Background(), input)
			if !errors.Is(err, domain.ErrInvalidRequest) {
				t.Fatalf("want ErrInvalidRequest, got %v", err)
			}
		})
	}
	if n := testutil.Count(t, pool, `SELECT COUNT(*) FROM transfers`); n != 0 {
		t.Errorf("nothing should be stored, got %d transfers", n)
	}
}

// ------------------------------------------------------------ idempotency

func TestDuplicateKeyReturnsOriginalWithoutSecondTransfer(t *testing.T) {
	svc, pool := setup(t)
	testutil.SeedWallet(t, pool, "w1", 500)
	testutil.SeedWallet(t, pool, "w2", 0)

	first, err := svc.CreateTransfer(context.Background(), in("k1", "w1", "w2", 100))
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.CreateTransfer(context.Background(), in("k1", "w1", "w2", 100))
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || second.Transfer.ID != first.Transfer.ID {
		t.Fatalf("second call must be a replay of the first: %+v vs %+v", second, first)
	}
	if b := testutil.BalanceOf(t, pool, "w1"); b != 400 {
		t.Errorf("w1 = %d, want 400 (charged only once)", b)
	}
	if n := testutil.Count(t, pool, `SELECT COUNT(*) FROM ledger_entries`); n != 2 {
		t.Errorf("want exactly 2 ledger entries, got %d", n)
	}
	testutil.AssertLedgerConsistent(t, pool)
}

func TestDuplicateKeyAfterFailureReturnsSameFailedResult(t *testing.T) {
	svc, pool := setup(t)
	testutil.SeedWallet(t, pool, "w1", 10)
	testutil.SeedWallet(t, pool, "w2", 0)

	first, _ := svc.CreateTransfer(context.Background(), in("k1", "w1", "w2", 100))
	second, err := svc.CreateTransfer(context.Background(), in("k1", "w1", "w2", 100))
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || second.Transfer.Status != domain.StatusFailed || second.Transfer.ID != first.Transfer.ID {
		t.Fatalf("want replayed FAILED result, got %+v", second)
	}
}

func TestSameKeyDifferentPayloadIsRejected(t *testing.T) {
	svc, pool := setup(t)
	testutil.SeedWallet(t, pool, "w1", 500)
	testutil.SeedWallet(t, pool, "w2", 0)

	if _, err := svc.CreateTransfer(context.Background(), in("k1", "w1", "w2", 100)); err != nil {
		t.Fatal(err)
	}
	_, err := svc.CreateTransfer(context.Background(), in("k1", "w1", "w2", 999))
	if !errors.Is(err, domain.ErrIdempotencyKeyReuse) {
		t.Fatalf("want ErrIdempotencyKeyReuse, got %v", err)
	}
	if b := testutil.BalanceOf(t, pool, "w1"); b != 400 {
		t.Errorf("w1 = %d, want 400", b)
	}
}

// ------------------------------------------------------------ concurrency

func TestNoDoubleSpendUnderConcurrentDebits(t *testing.T) {
	svc, pool := setup(t)
	testutil.SeedWallet(t, pool, "w1", 100)
	testutil.SeedWallet(t, pool, "w2", 0)

	// 20 parallel transfers of 10 from a wallet that has 100 => exactly 10 may succeed.
	errs := runParallel(20, func(i int) error {
		_, err := svc.CreateTransfer(context.Background(), in(fmt.Sprintf("k%d", i), "w1", "w2", 10))
		return err
	})
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if b := testutil.BalanceOf(t, pool, "w1"); b != 0 {
		t.Errorf("w1 = %d, want 0", b)
	}
	if b := testutil.BalanceOf(t, pool, "w2"); b != 100 {
		t.Errorf("w2 = %d, want 100", b)
	}
	if n := testutil.Count(t, pool, `SELECT COUNT(*) FROM transfers WHERE status='PROCESSED'`); n != 10 {
		t.Errorf("PROCESSED = %d, want 10", n)
	}
	if n := testutil.Count(t, pool, `SELECT COUNT(*) FROM transfers WHERE status='FAILED'`); n != 10 {
		t.Errorf("FAILED = %d, want 10", n)
	}
	testutil.AssertLedgerConsistent(t, pool)
}

func TestConcurrentDuplicateRequestsExecuteOnce(t *testing.T) {
	svc, pool := setup(t)
	testutil.SeedWallet(t, pool, "w1", 1000)
	testutil.SeedWallet(t, pool, "w2", 0)

	var mu sync.Mutex
	var results []*service.Result
	errs := runParallel(10, func(i int) error {
		r, err := svc.CreateTransfer(context.Background(), in("same-key", "w1", "w2", 100))
		if err == nil {
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		}
		return err
	})
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	ids := map[string]bool{}
	fresh := 0
	for _, r := range results {
		ids[r.Transfer.ID] = true
		if !r.Replayed {
			fresh++
		}
	}
	if len(ids) != 1 {
		t.Errorf("all responses must refer to ONE transfer, got %d", len(ids))
	}
	if fresh != 1 {
		t.Errorf("exactly one request should execute, got %d", fresh)
	}
	if b := testutil.BalanceOf(t, pool, "w1"); b != 900 {
		t.Errorf("w1 = %d, want 900 (charged once)", b)
	}
	testutil.AssertLedgerConsistent(t, pool)
}

func TestOppositeDirectionTransfersDoNotDeadlock(t *testing.T) {
	svc, pool := setup(t)
	testutil.SeedWallet(t, pool, "a", 1000)
	testutil.SeedWallet(t, pool, "b", 1000)

	errs := runParallel(20, func(i int) error {
		from, to := "a", "b"
		if i%2 == 1 {
			from, to = "b", "a"
		}
		_, err := svc.CreateTransfer(context.Background(), in(fmt.Sprintf("k%d", i), from, to, 10))
		return err
	})
	if len(errs) > 0 {
		t.Fatalf("unexpected errors (deadlock?): %v", errs)
	}
	if sum := testutil.BalanceOf(t, pool, "a") + testutil.BalanceOf(t, pool, "b"); sum != 2000 {
		t.Errorf("money was created or lost: total=%d, want 2000", sum)
	}
	testutil.AssertLedgerConsistent(t, pool)
}
