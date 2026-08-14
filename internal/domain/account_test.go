package domain

import (
	"errors"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The brief's canonical deployment: 1,000,000 naira over 50 weeks, which is
// 20,000 naira a week and divides evenly.
const (
	testPayable = Kobo(100_000_000)
	testWeekly  = Kobo(2_000_000)
	testTerm    = 50
)

var testStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func testAccount(paid Kobo) Account {
	return Account{
		ID:           uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		CustomerID:   "GIG00001",
		AssetValue:   80_000_000,
		TotalPayable: testPayable,
		WeeklyDue:    testWeekly,
		TotalPaid:    paid,
		TermWeeks:    testTerm,
		StartDate:    testStart,
		Status:       StatusActive,
	}
}

func TestWeeklyDue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payable Kobo
		term    int
		want    Kobo
		err     error
	}{
		{name: "the brief's deployment divides evenly", payable: 100_000_000, term: 50, want: 2_000_000},
		{name: "floors when it does not divide", payable: 100_000_007, term: 50, want: 2_000_000},
		{name: "single week", payable: 5_000, term: 1, want: 5_000},
		{name: "zero term", payable: 100_000_000, term: 0, err: ErrInvalidTerm},
		{name: "negative term", payable: 100_000_000, term: -1, err: ErrInvalidTerm},
		{name: "zero obligation", payable: 0, term: 50, err: ErrAmountNotPositive},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := WeeklyDue(tc.payable, tc.term)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("WeeklyDue(%d, %d) error = %v, want %v", tc.payable, tc.term, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("WeeklyDue(%d, %d) unexpected error: %v", tc.payable, tc.term, err)
			}
			if got != tc.want {
				t.Errorf("WeeklyDue(%d, %d) = %d, want %d", tc.payable, tc.term, got, tc.want)
			}
		})
	}
}

// TestInstalmentsSumToObligation is the property that makes the rounding rule
// safe: whatever the term, the instalments must add up to exactly the amount
// owed. A floor that loses the remainder would quietly forgive debt; a ceiling
// would overcharge. Neither is acceptable, so the final week absorbs it.
func TestInstalmentsSumToObligation(t *testing.T) {
	t.Parallel()

	payables := []Kobo{100_000_000, 100_000_007, 99_999_999, 1, 7, 123_456_789}
	terms := []int{1, 2, 7, 50, 52, 999}

	for _, payable := range payables {
		for _, term := range terms {
			weekly, err := WeeklyDue(payable, term)
			if err != nil {
				continue // covered by TestWeeklyDue
			}
			final, err := FinalInstalment(payable, term)
			if err != nil {
				t.Fatalf("FinalInstalment(%d, %d): %v", payable, term, err)
			}

			total := weekly*Kobo(term-1) + final
			if total != payable {
				t.Errorf("payable=%d term=%d: instalments sum to %d, want %d", payable, term, total, payable)
			}
			if final <= 0 {
				t.Errorf("payable=%d term=%d: final instalment %d must be positive", payable, term, final)
			}
			// The remainder lands in the final week, so it is never smaller than
			// a regular week -- a customer is never asked for a token last payment.
			if term > 1 && final < weekly {
				t.Errorf("payable=%d term=%d: final %d < weekly %d", payable, term, final, weekly)
			}
		}
	}
}

func TestAllocate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		account Account
		amount  Kobo
		want    Allocation
		err     error
	}{
		{
			name:    "one full instalment",
			account: testAccount(0),
			amount:  2_000_000,
			want: Allocation{
				Applied: 2_000_000, Excess: 0, Settles: false,
				NewTotalPaid: 2_000_000, NewOverpayment: 0, NewOutstanding: 98_000_000,
				NewStatus: StatusActive, InstalmentsMet: 1, PartPaidCurrent: 0,
			},
		},
		{
			name:    "underpayment part-fills the current instalment",
			account: testAccount(0),
			amount:  1_000_000,
			want: Allocation{
				Applied: 1_000_000, NewTotalPaid: 1_000_000, NewOutstanding: 99_000_000,
				NewStatus: StatusActive, InstalmentsMet: 0, PartPaidCurrent: 1_000_000,
			},
		},
		{
			name:    "FIFO: a large payment settles several oldest instalments",
			account: testAccount(0),
			amount:  10_000_000,
			want: Allocation{
				Applied: 10_000_000, NewTotalPaid: 10_000_000, NewOutstanding: 90_000_000,
				NewStatus: StatusActive, InstalmentsMet: 5, PartPaidCurrent: 0,
			},
		},
		{
			name:    "final instalment settles the deployment exactly",
			account: testAccount(98_000_000),
			amount:  2_000_000,
			want: Allocation{
				Applied: 2_000_000, Settles: true,
				NewTotalPaid: 100_000_000, NewOutstanding: 0,
				NewStatus: StatusCompleted, InstalmentsMet: 50, PartPaidCurrent: 0,
			},
		},
		{
			name:    "overpayment settles the obligation and banks the excess",
			account: testAccount(99_000_000),
			amount:  2_000_000,
			want: Allocation{
				Applied: 1_000_000, Excess: 1_000_000, Settles: true,
				NewTotalPaid: 100_000_000, NewOverpayment: 1_000_000, NewOutstanding: 0,
				NewStatus: StatusCompleted, InstalmentsMet: 50, PartPaidCurrent: 0,
			},
		},
		{
			name:    "paying off the whole deployment in one transfer",
			account: testAccount(0),
			amount:  100_000_000,
			want: Allocation{
				Applied: 100_000_000, Settles: true,
				NewTotalPaid: 100_000_000, NewOutstanding: 0,
				NewStatus: StatusCompleted, InstalmentsMet: 50,
			},
		},

		{name: "zero amount", account: testAccount(0), amount: 0, err: ErrAmountNotPositive},
		{name: "negative amount", account: testAccount(0), amount: -1, err: ErrAmountNotPositive},
		{
			// Routed to suspense by the caller rather than absorbed: with one
			// deployment at a time there is nothing for it to pay down, and
			// silently banking it would hide a real operational problem.
			name:    "already completed",
			account: func() Account { a := testAccount(100_000_000); a.Status = StatusCompleted; return a }(),
			amount:  2_000_000,
			err:     ErrAccountNotActive,
		},
		{
			name:    "written off",
			account: func() Account { a := testAccount(10_000_000); a.Status = StatusWrittenOff; return a }(),
			amount:  2_000_000,
			err:     ErrAccountNotActive,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := tc.account.Allocate(tc.amount)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("Allocate(%d) error = %v, want %v", tc.amount, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Allocate(%d) unexpected error: %v", tc.amount, err)
			}
			if got != tc.want {
				t.Errorf("Allocate(%d)\n got: %+v\nwant: %+v", tc.amount, got, tc.want)
			}
		})
	}
}

// TestAllocateRemainderTerm exercises the branch where the final instalment
// carries a rounding remainder: every earlier week is settled but the obligation
// is not yet discharged, so the account must not report itself complete.
func TestAllocateRemainderTerm(t *testing.T) {
	t.Parallel()

	acct := testAccount(0)
	acct.TotalPayable = 100_000_007
	acct.WeeklyDue = 2_000_000 // floor; the final week owes 2,000,007

	alloc, err := acct.Allocate(100_000_000)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}

	if alloc.Settles {
		t.Error("Settles = true, but 7 kobo remain outstanding")
	}
	if alloc.NewOutstanding != 7 {
		t.Errorf("NewOutstanding = %d, want 7", alloc.NewOutstanding)
	}
	if alloc.NewStatus != StatusActive {
		t.Errorf("NewStatus = %q, want %q", alloc.NewStatus, StatusActive)
	}
	// 49 whole weeks settled, with 2,000,000 sitting against the 2,000,007 final.
	if alloc.InstalmentsMet != 49 {
		t.Errorf("InstalmentsMet = %d, want 49", alloc.InstalmentsMet)
	}
	if alloc.PartPaidCurrent != 2_000_000 {
		t.Errorf("PartPaidCurrent = %d, want 2000000", alloc.PartPaidCurrent)
	}
}

func TestPositionAt(t *testing.T) {
	t.Parallel()

	day := func(n int) time.Time { return testStart.AddDate(0, 0, n) }

	tests := []struct {
		name    string
		account Account
		now     time.Time
		want    Position
	}{
		{
			name:    "day of deployment: nothing is due yet",
			account: testAccount(0),
			now:     testStart,
			want:    Position{WeeksElapsed: 0, ExpectedToDate: 0, Arrears: 0, WeeksBehind: 0, NextDueWeek: 1},
		},
		{
			name:    "day six: the first instalment has still not fallen due",
			account: testAccount(0),
			now:     day(6),
			want:    Position{WeeksElapsed: 0, ExpectedToDate: 0, NextDueWeek: 1},
		},
		{
			name:    "day seven, unpaid: one week behind",
			account: testAccount(0),
			now:     day(7),
			want:    Position{WeeksElapsed: 1, ExpectedToDate: 2_000_000, Arrears: 2_000_000, WeeksBehind: 1, NextDueWeek: 1},
		},
		{
			name:    "paid on schedule",
			account: testAccount(2_000_000),
			now:     day(7),
			want:    Position{WeeksElapsed: 1, ExpectedToDate: 2_000_000, InstalmentsMet: 1, NextDueWeek: 2},
		},
		{
			name:    "prepaid five weeks in the first week",
			account: testAccount(10_000_000),
			now:     day(7),
			want:    Position{WeeksElapsed: 1, ExpectedToDate: 2_000_000, AheadBy: 8_000_000, InstalmentsMet: 5, NextDueWeek: 6},
		},
		{
			name:    "part of a week overdue still counts as a week behind",
			account: testAccount(5_000_000),
			now:     day(21),
			want: Position{
				WeeksElapsed: 3, ExpectedToDate: 6_000_000, Arrears: 1_000_000, WeeksBehind: 1,
				InstalmentsMet: 2, PartPaidCurrent: 1_000_000, NextDueWeek: 3,
			},
		},
		{
			name:    "term fully run, nothing paid: the whole obligation is due",
			account: testAccount(0),
			now:     day(400),
			want: Position{
				WeeksElapsed: 57, ExpectedToDate: 100_000_000, Arrears: 100_000_000,
				WeeksBehind: 50, NextDueWeek: 1,
			},
		},
		{
			name: "completed deployment",
			account: func() Account {
				a := testAccount(100_000_000)
				a.Status = StatusCompleted
				return a
			}(),
			now:  day(350),
			want: Position{WeeksElapsed: 50, ExpectedToDate: 100_000_000, InstalmentsMet: 50, NextDueWeek: 0},
		},
		{
			name:    "clock behind the start date never reports negative elapsed weeks",
			account: testAccount(0),
			now:     day(-30),
			want:    Position{WeeksElapsed: 0, ExpectedToDate: 0, NextDueWeek: 1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := tc.account.PositionAt(tc.now)

			check := func(field string, got, want any) {
				t.Helper()
				if got != want {
					t.Errorf("%s = %v, want %v", field, got, want)
				}
			}
			check("WeeksElapsed", got.WeeksElapsed, tc.want.WeeksElapsed)
			check("ExpectedToDate", got.ExpectedToDate, tc.want.ExpectedToDate)
			check("Arrears", got.Arrears, tc.want.Arrears)
			check("AheadBy", got.AheadBy, tc.want.AheadBy)
			check("WeeksBehind", got.WeeksBehind, tc.want.WeeksBehind)
			check("InstalmentsMet", got.InstalmentsMet, tc.want.InstalmentsMet)
			check("PartPaidCurrent", got.PartPaidCurrent, tc.want.PartPaidCurrent)
			check("NextDueWeek", got.NextDueWeek, tc.want.NextDueWeek)

			// Holds for every account in every state.
			if got.Outstanding < 0 {
				t.Errorf("Outstanding = %d, must never be negative", got.Outstanding)
			}
			if got.Arrears > 0 && got.AheadBy > 0 {
				t.Error("a customer cannot be both behind and ahead")
			}
		})
	}
}

// TestAllocateOrderIndependence is the property that makes out-of-order arrival
// a non-issue: bank notifications are not guaranteed to arrive in the order the
// transfers settled, so the final balance must not depend on it.
func TestAllocateOrderIndependence(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(42, 1024))

	for trial := 0; trial < 500; trial++ {
		n := 1 + rng.IntN(12)
		amounts := make([]Kobo, n)
		for i := range amounts {
			amounts[i] = Kobo(1 + rng.Int64N(30_000_000))
		}

		shuffled := make([]Kobo, n)
		copy(shuffled, amounts)
		rng.Shuffle(n, func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

		a := applyAll(t, testAccount(0), amounts)
		b := applyAll(t, testAccount(0), shuffled)

		if a.TotalPaid != b.TotalPaid {
			t.Fatalf("trial %d: TotalPaid depends on arrival order: %d vs %d (amounts %v)", trial, a.TotalPaid, b.TotalPaid, amounts)
		}
		if a.Status != b.Status {
			t.Fatalf("trial %d: Status depends on arrival order: %q vs %q", trial, a.Status, b.Status)
		}
	}
}

// TestOverpaymentDependsOnArrivalOrder documents a real asymmetry rather than an
// oversight. Excess only materialises in the payment that settles the account;
// a payment arriving afterwards finds no active deployment and is rejected here,
// to be routed to suspense by the caller. Both outcomes are correct, and both
// keep the money accounted for -- but they are different rows, so the test
// pins the behaviour down.
func TestOverpaymentDependsOnArrivalOrder(t *testing.T) {
	t.Parallel()

	settledFirst := applyAll(t, testAccount(0), []Kobo{100_000_000, 5_000_000})
	if settledFirst.Overpayment != 0 {
		t.Errorf("Overpayment = %d, want 0: the trailing payment finds no active deployment", settledFirst.Overpayment)
	}

	overshotOnSettle := applyAll(t, testAccount(0), []Kobo{50_000_000, 55_000_000})
	if overshotOnSettle.Overpayment != 5_000_000 {
		t.Errorf("Overpayment = %d, want 5000000: the settling payment carries the excess", overshotOnSettle.Overpayment)
	}
}

// applyAll folds payments into an account, asserting the per-payment invariants
// on the way through.
func applyAll(t *testing.T, acct Account, amounts []Kobo) Account {
	t.Helper()

	for _, amount := range amounts {
		alloc, err := acct.Allocate(amount)
		if err != nil {
			// Only legitimate reason to skip: the account already settled.
			if !errors.Is(err, ErrAccountNotActive) {
				t.Fatalf("Allocate(%d): unexpected error %v", amount, err)
			}
			continue
		}

		if alloc.Applied+alloc.Excess != amount {
			t.Fatalf("money vanished: applied %d + excess %d != amount %d", alloc.Applied, alloc.Excess, amount)
		}
		if alloc.NewOutstanding < 0 {
			t.Fatalf("outstanding went negative: %d", alloc.NewOutstanding)
		}
		if alloc.NewTotalPaid > acct.TotalPayable {
			t.Fatalf("total paid %d exceeds obligation %d", alloc.NewTotalPaid, acct.TotalPayable)
		}

		acct.TotalPaid = alloc.NewTotalPaid
		acct.Overpayment = alloc.NewOverpayment
		acct.Status = alloc.NewStatus
		acct.Version++
	}
	return acct
}
