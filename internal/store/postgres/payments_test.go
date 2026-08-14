package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zabilal/gigmile-facility/internal/domain"
)

// These tests run against a real Postgres rather than a mock. A mock would
// verify that we call the functions we wrote, which is not in doubt; what is in
// doubt is whether the unique index, the CHECK constraints, the row locking and
// the transaction boundaries behave as designed under concurrency. Only the
// database can answer that.

const (
	testPayable = domain.Kobo(100_000_000) // 1,000,000 naira
	testWeekly  = domain.Kobo(2_000_000)   // 20,000 naira
	testTerm    = 50
)

func testStore(t *testing.T) *Store {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		url = "postgres://facility:facility@localhost:5433/facility?sslmode=disable"
	}

	ctx := context.Background()
	store, err := New(ctx, url, 40)
	if err != nil {
		t.Skipf("integration tests need Postgres (run `make up`): %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

// newCustomer returns an identifier unique to this test, so tests share a
// database without sharing state and can run in parallel without truncation.
func newCustomer(t *testing.T) string {
	t.Helper()
	return "GIG" + uuid.NewString()[:8]
}

func deploy(t *testing.T, s *Store, customerID string) domain.Account {
	t.Helper()

	acct, err := s.Deploy(context.Background(), Deployment{
		CustomerID:   customerID,
		AssetValue:   80_000_000,
		TotalPayable: testPayable,
		TermWeeks:    testTerm,
		StartDate:    time.Now().AddDate(0, 0, -7),
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	return acct
}

func notify(customerID, reference string, amount domain.Kobo) (domain.Notification, []byte) {
	n := domain.Notification{
		Reference:     reference,
		CustomerID:    customerID,
		Amount:        amount,
		Status:        domain.PaymentComplete,
		TransactionAt: time.Now(),
	}
	raw, _ := json.Marshal(map[string]string{
		"customer_id":           customerID,
		"payment_status":        "COMPLETE",
		"transaction_amount":    amount.String(),
		"transaction_reference": reference,
	})
	return n, raw
}

func apply(t *testing.T, s *Store, customerID, reference string, amount domain.Kobo) Result {
	t.Helper()

	n, raw := notify(customerID, reference, amount)
	res, err := s.ApplyPayment(context.Background(), n, raw, time.Now())
	if err != nil {
		t.Fatalf("ApplyPayment(%s, %s): %v", reference, amount, err)
	}
	return res
}

func TestApplyPaymentHappyPath(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	customer := newCustomer(t)
	acct := deploy(t, s, customer)

	res := apply(t, s, customer, "REF-"+uuid.NewString(), testWeekly)

	if res.Outcome != OutcomeApplied {
		t.Fatalf("Outcome = %q, want %q (%s)", res.Outcome, OutcomeApplied, res.Reason)
	}
	if res.Applied != testWeekly || res.Excess != 0 {
		t.Errorf("applied %s excess %s, want %s and 0", res.Applied, res.Excess, testWeekly)
	}
	if res.Position == nil {
		t.Fatal("Position is nil: the caller should not need a second round trip")
	}
	if got, want := res.Position.Outstanding, testPayable-testWeekly; got != want {
		t.Errorf("Outstanding = %s, want %s", got, want)
	}
	if got := res.Position.InstalmentsMet; got != 1 {
		t.Errorf("InstalmentsMet = %d, want 1", got)
	}

	assertReconciled(t, s, acct.ID)
}

// TestIdempotencySequential: a provider retrying a delivered webhook must not be
// charged twice.
func TestIdempotencySequential(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	customer := newCustomer(t)
	acct := deploy(t, s, customer)
	ref := "REF-" + uuid.NewString()

	first := apply(t, s, customer, ref, testWeekly)
	if first.Outcome != OutcomeApplied {
		t.Fatalf("first outcome = %q, want applied", first.Outcome)
	}

	for i := 0; i < 5; i++ {
		repeat := apply(t, s, customer, ref, testWeekly)
		if repeat.Outcome != OutcomeDuplicate {
			t.Fatalf("retry %d outcome = %q, want %q", i, repeat.Outcome, OutcomeDuplicate)
		}
		if repeat.PaymentID != first.PaymentID {
			t.Errorf("retry %d resolved to payment %d, want the original %d", i, repeat.PaymentID, first.PaymentID)
		}
		// A retry must report the same position, not a moved one.
		if repeat.Position == nil || repeat.Position.TotalPaid != testWeekly {
			t.Errorf("retry %d reported a changed balance", i)
		}
	}

	if n := countLedgerEntries(t, s, acct.ID); n != 1 {
		t.Errorf("ledger has %d entries after 6 deliveries of one payment, want 1", n)
	}
	assertReconciled(t, s, acct.ID)
}

// TestIdempotencyConcurrent is the test that a "SELECT then INSERT if absent"
// implementation fails: the check and the write are not atomic, so simultaneous
// deliveries both find nothing and both apply.
func TestIdempotencyConcurrent(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	customer := newCustomer(t)
	acct := deploy(t, s, customer)
	ref := "REF-" + uuid.NewString()

	const deliveries = 60
	var applied, duplicate atomic.Int64

	var wg sync.WaitGroup
	for i := 0; i < deliveries; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			n, raw := notify(customer, ref, testWeekly)
			res, err := s.ApplyPayment(context.Background(), n, raw, time.Now())
			if err != nil {
				t.Errorf("ApplyPayment: %v", err)
				return
			}
			switch res.Outcome {
			case OutcomeApplied:
				applied.Add(1)
			case OutcomeDuplicate:
				duplicate.Add(1)
			default:
				t.Errorf("unexpected outcome %q", res.Outcome)
			}
		}()
	}
	wg.Wait()

	if applied.Load() != 1 {
		t.Errorf("%d concurrent deliveries applied %d times, want exactly 1", deliveries, applied.Load())
	}
	if duplicate.Load() != deliveries-1 {
		t.Errorf("duplicates = %d, want %d", duplicate.Load(), deliveries-1)
	}
	if n := countLedgerEntries(t, s, acct.ID); n != 1 {
		t.Errorf("ledger has %d entries, want 1", n)
	}
	assertReconciled(t, s, acct.ID)
}

// TestConcurrentDistinctPaymentsDoNotLoseUpdates is the lost-update test. A
// read-modify-write in application code passes every sequential test and fails
// this one: two payments read the same balance and the second overwrites the
// first's increment.
func TestConcurrentDistinctPaymentsDoNotLoseUpdates(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	customer := newCustomer(t)
	acct := deploy(t, s, customer)

	// 50 instalments of 20,000 naira settle a 1,000,000 naira deployment exactly.
	const payments = testTerm
	var wg sync.WaitGroup
	for i := 0; i < payments; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			n, raw := notify(customer, fmt.Sprintf("REF-%s-%02d", acct.ID, i), testWeekly)
			if _, err := s.ApplyPayment(context.Background(), n, raw, time.Now()); err != nil {
				t.Errorf("payment %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	final, err := loadAccountByID(context.Background(), s.pool, acct.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if final.TotalPaid != testPayable {
		t.Errorf("TotalPaid = %s after %d concurrent payments, want %s -- updates were lost",
			final.TotalPaid, payments, testPayable)
	}
	if final.Status != domain.StatusCompleted {
		t.Errorf("Status = %q, want %q", final.Status, domain.StatusCompleted)
	}
	if n := countLedgerEntries(t, s, acct.ID); n != payments {
		t.Errorf("ledger has %d entries, want %d", n, payments)
	}
	assertReconciled(t, s, acct.ID)
}

func TestAmountMismatchIsNotADuplicate(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	customer := newCustomer(t)
	acct := deploy(t, s, customer)
	ref := "REF-" + uuid.NewString()

	apply(t, s, customer, ref, testWeekly)

	// Same reference, larger amount: a provider defect, or someone testing
	// whether replaying a reference clears a debt. Neither may be applied.
	n, raw := notify(customer, ref, 50_000_000)
	_, err := s.ApplyPayment(context.Background(), n, raw, time.Now())
	if !errors.Is(err, ErrAmountMismatch) {
		t.Fatalf("error = %v, want ErrAmountMismatch", err)
	}

	final, err := loadAccountByID(context.Background(), s.pool, acct.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if final.TotalPaid != testWeekly {
		t.Errorf("TotalPaid = %s, want %s: the mismatched replay must not move money", final.TotalPaid, testWeekly)
	}
	assertReconciled(t, s, acct.ID)
}

func TestDispositions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status domain.PaymentStatus
		want   Outcome
	}{
		{"pending is recorded but not applied", domain.PaymentPending, OutcomeIgnored},
		{"failed is recorded but not applied", domain.PaymentFailed, OutcomeIgnored},
		{"reversed needs a human", domain.PaymentReversed, OutcomeSuspense},
		{"an unmodelled status is never guessed at", domain.PaymentStatus("SETTLED_MAYBE"), OutcomeSuspense},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := testStore(t)
			customer := newCustomer(t)
			acct := deploy(t, s, customer)

			n, raw := notify(customer, "REF-"+uuid.NewString(), testWeekly)
			n.Status = tc.status

			res, err := s.ApplyPayment(context.Background(), n, raw, time.Now())
			if err != nil {
				t.Fatalf("ApplyPayment: %v", err)
			}
			if res.Outcome != tc.want {
				t.Errorf("Outcome = %q, want %q", res.Outcome, tc.want)
			}
			if res.Reason == "" {
				t.Error("Reason is empty: ops cannot triage a queue with no explanation")
			}
			if n := countLedgerEntries(t, s, acct.ID); n != 0 {
				t.Errorf("ledger has %d entries, want 0: nothing settled", n)
			}
		})
	}
}

func TestNoActiveDeploymentGoesToSuspense(t *testing.T) {
	t.Parallel()

	s := testStore(t)

	// A customer we have never deployed to. The money arrived; discarding it
	// because we cannot map it would be the worst available outcome.
	res := apply(t, s, newCustomer(t), "REF-"+uuid.NewString(), testWeekly)

	if res.Outcome != OutcomeSuspense {
		t.Errorf("Outcome = %q, want %q", res.Outcome, OutcomeSuspense)
	}
	if res.PaymentID == 0 {
		t.Error("payment was not recorded; an unmatched credit must still be persisted for reconciliation")
	}
}

func TestOverpaymentSettlesAndBanksExcess(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	customer := newCustomer(t)
	acct := deploy(t, s, customer)

	apply(t, s, customer, "REF-"+uuid.NewString(), 99_000_000)

	// 20,000 naira against 10,000 naira outstanding.
	res := apply(t, s, customer, "REF-"+uuid.NewString(), testWeekly)
	if res.Outcome != OutcomeApplied {
		t.Fatalf("Outcome = %q, want applied", res.Outcome)
	}
	if res.Applied != 1_000_000 || res.Excess != 1_000_000 {
		t.Errorf("applied %s excess %s, want 10000.00 and 10000.00", res.Applied, res.Excess)
	}

	final, err := loadAccountByID(context.Background(), s.pool, acct.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if final.Status != domain.StatusCompleted {
		t.Errorf("Status = %q, want COMPLETED", final.Status)
	}
	if final.TotalPaid != testPayable {
		t.Errorf("TotalPaid = %s, want %s", final.TotalPaid, testPayable)
	}
	if final.Overpayment != 1_000_000 {
		t.Errorf("Overpayment = %s, want 10000.00: excess is banked, never discarded", final.Overpayment)
	}

	// A further payment now has no active deployment to settle against.
	after := apply(t, s, customer, "REF-"+uuid.NewString(), testWeekly)
	if after.Outcome != OutcomeSuspense {
		t.Errorf("post-completion payment outcome = %q, want suspense", after.Outcome)
	}

	assertReconciled(t, s, acct.ID)
}

// TestSQLAllocationMatchesDomain guards the one duplication in this design: the
// allocation rule is expressed both in domain.Allocate (tested exhaustively,
// with no database) and in applyPaymentSQL (fast, atomic, no read-modify-write).
// Keeping both is a deliberate trade -- but only if they cannot silently drift,
// which is what this test enforces.
func TestSQLAllocationMatchesDomain(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		prime  domain.Kobo // paid before the payment under test
		amount domain.Kobo
	}{
		{"first instalment", 0, testWeekly},
		{"underpayment", 0, 1_000_000},
		{"several instalments at once", 0, 10_000_000},
		{"settles exactly", 98_000_000, testWeekly},
		{"overshoots", 99_000_000, testWeekly},
		{"clears the whole obligation", 0, testPayable},
		{"one kobo", 0, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := testStore(t)
			customer := newCustomer(t)
			acct := deploy(t, s, customer)

			if tc.prime > 0 {
				apply(t, s, customer, "PRIME-"+uuid.NewString(), tc.prime)
			}

			before, err := loadAccountByID(context.Background(), s.pool, acct.ID)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}

			want, err := before.Allocate(tc.amount)
			if err != nil {
				t.Fatalf("domain.Allocate: %v", err)
			}

			got := apply(t, s, customer, "REF-"+uuid.NewString(), tc.amount)
			after, err := loadAccountByID(context.Background(), s.pool, acct.ID)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}

			if got.Applied != want.Applied {
				t.Errorf("applied: SQL %s, domain %s", got.Applied, want.Applied)
			}
			if got.Excess != want.Excess {
				t.Errorf("excess: SQL %s, domain %s", got.Excess, want.Excess)
			}
			if after.TotalPaid != want.NewTotalPaid {
				t.Errorf("total paid: SQL %s, domain %s", after.TotalPaid, want.NewTotalPaid)
			}
			if after.Overpayment != want.NewOverpayment {
				t.Errorf("overpayment: SQL %s, domain %s", after.Overpayment, want.NewOverpayment)
			}
			if after.Status != want.NewStatus {
				t.Errorf("status: SQL %q, domain %q", after.Status, want.NewStatus)
			}
		})
	}
}

func TestOneActiveDeploymentPerCustomer(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	customer := newCustomer(t)
	deploy(t, s, customer)

	_, err := s.Deploy(context.Background(), Deployment{
		CustomerID:   customer,
		AssetValue:   80_000_000,
		TotalPayable: testPayable,
		TermWeeks:    testTerm,
		StartDate:    time.Now(),
	})
	if !errors.Is(err, ErrAlreadyDeployed) {
		t.Fatalf("second deployment error = %v, want ErrAlreadyDeployed", err)
	}
}

// TestLedgerIsAppendOnly proves the immutability trigger, not just its presence.
func TestLedgerIsAppendOnly(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := testStore(t)
	customer := newCustomer(t)
	acct := deploy(t, s, customer)
	apply(t, s, customer, "REF-"+uuid.NewString(), testWeekly)

	if _, err := s.pool.Exec(ctx,
		`UPDATE ledger_entries SET amount_kobo = 1 WHERE account_id = $1`, acct.ID); err == nil {
		t.Error("UPDATE on ledger_entries succeeded; history must not be rewritable")
	}
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM ledger_entries WHERE account_id = $1`, acct.ID); err == nil {
		t.Error("DELETE on ledger_entries succeeded; history must not be erasable")
	}

	assertReconciled(t, s, acct.ID)
}

func TestLedgerKeysetPagination(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := testStore(t)
	customer := newCustomer(t)
	acct := deploy(t, s, customer)

	const total = 7
	for i := 0; i < total; i++ {
		apply(t, s, customer, fmt.Sprintf("REF-%s-%d", acct.ID, i), domain.Kobo(1_000_000+i))
	}

	var (
		seen   []int64
		cursor *int64
	)
	for {
		page, err := s.Ledger(ctx, customer, cursor, 3)
		if err != nil {
			t.Fatalf("Ledger: %v", err)
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			if len(seen) > 0 && e.ID >= seen[len(seen)-1] {
				t.Fatalf("entries out of order: %d after %d", e.ID, seen[len(seen)-1])
			}
			seen = append(seen, e.ID)
		}
		last := page[len(page)-1].ID
		cursor = &last
	}

	if len(seen) != total {
		t.Errorf("paged through %d entries, want %d", len(seen), total)
	}
}

// TestLedgerIncludesEntriesWithoutAPayment is a regression test.
//
// Opening balances, adjustments and write-offs have no originating payment, so
// an inner join against payments drops them from the statement. The balance
// still moves, the ledger still holds the entry, and the customer's statement
// silently stops adding up -- the failure is invisible until someone disputes a
// figure. The statement must sum to the balance it explains.
func TestLedgerIncludesEntriesWithoutAPayment(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := testStore(t)
	customer := newCustomer(t)

	const opening = domain.Kobo(40_000_000)
	acct, err := s.Deploy(ctx, Deployment{
		CustomerID:   customer,
		AssetValue:   80_000_000,
		TotalPayable: testPayable,
		TermWeeks:    testTerm,
		StartDate:    time.Now().AddDate(0, 0, -140),
		PriorPaid:    opening, // onboarded mid-term from another system
	})
	if err != nil {
		t.Fatalf("deploy with opening balance: %v", err)
	}

	apply(t, s, customer, "REF-"+uuid.NewString(), testWeekly)

	entries, err := s.Ledger(ctx, customer, nil, 50)
	if err != nil {
		t.Fatalf("Ledger: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("statement has %d entries, want 2 (opening adjustment + repayment)", len(entries))
	}

	var (
		total    domain.Kobo
		sawOpen  bool
		sawRepay bool
	)
	for _, e := range entries {
		total += e.Amount
		switch e.EntryType {
		case "ADJUSTMENT":
			sawOpen = true
			if e.Reference != "" {
				t.Errorf("adjustment carries reference %q; it originates in no payment", e.Reference)
			}
		case "REPAYMENT":
			sawRepay = true
			if e.Reference == "" {
				t.Error("repayment is missing its transaction reference")
			}
		}
	}
	if !sawOpen || !sawRepay {
		t.Errorf("statement missing an entry type: opening=%v repayment=%v", sawOpen, sawRepay)
	}

	final, err := loadAccountByID(ctx, s.pool, acct.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if total != final.TotalPaid {
		t.Errorf("statement sums to %s but the balance says %s", total, final.TotalPaid)
	}
	assertReconciled(t, s, acct.ID)
}

func TestPositionForUnknownCustomer(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	_, err := s.Position(context.Background(), newCustomer(t), time.Now())
	if !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("error = %v, want ErrAccountNotFound", err)
	}
}

// assertReconciled asserts invariant 2: the materialised balance equals the sum
// of the ledger. Every test that moves money checks it, because a balance that
// has drifted from its ledger is the failure this whole design exists to prevent.
func assertReconciled(t *testing.T, s *Store, accountID uuid.UUID) {
	t.Helper()

	ledgerTotal, materialised, err := s.ReconcileAccount(context.Background(), accountID)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if ledgerTotal != materialised {
		t.Errorf("ledger sums to %s but the balance says %s: the projection has drifted",
			ledgerTotal, materialised)
	}
}

func countLedgerEntries(t *testing.T, s *Store, accountID uuid.UUID) int {
	t.Helper()

	var n int
	err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ledger_entries WHERE account_id = $1`, accountID).Scan(&n)
	if err != nil {
		t.Fatalf("count ledger entries: %v", err)
	}
	return n
}
