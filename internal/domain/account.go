package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Status is the lifecycle state of a deployment.
type Status string

const (
	// StatusActive means the customer is still repaying.
	StatusActive Status = "ACTIVE"
	// StatusCompleted means the obligation is fully repaid and the asset is owned.
	StatusCompleted Status = "COMPLETED"
	// StatusWrittenOff means collection was abandoned; payments no longer apply.
	StatusWrittenOff Status = "WRITTEN_OFF"
)

// DaysPerWeek is the repayment cadence. Named rather than inlined because the
// week is a business unit here, not an arbitrary 7.
const DaysPerWeek = 7

var (
	// ErrAccountNotActive means the deployment cannot accept repayments.
	ErrAccountNotActive = errors.New("account is not active")
	// ErrInvalidTerm means the term was not a positive number of weeks.
	ErrInvalidTerm = errors.New("term must be a positive number of weeks")
)

// Account is the materialised position of a single asset deployment.
//
// A customer holds at most one ACTIVE account at a time; that rule is enforced
// by a partial unique index in the database rather than here, so it holds
// against every writer and not only against traffic arriving through the API.
type Account struct {
	ID           uuid.UUID
	CustomerID   string
	AssetValue   Kobo
	TotalPayable Kobo
	WeeklyDue    Kobo
	TotalPaid    Kobo
	Overpayment  Kobo
	TermWeeks    int
	StartDate    time.Time
	Status       Status
	Version      int64
}

// WeeklyDue computes the uniform instalment for a term.
//
// Integer division deliberately floors: 1,000,000 over 50 weeks divides evenly,
// but the general case does not, and a floored instalment with the remainder
// carried to the final week is the only split that never asks a customer for a
// fraction of a kobo and never leaves the obligation short. Use FinalInstalment
// for the last week's figure.
func WeeklyDue(totalPayable Kobo, termWeeks int) (Kobo, error) {
	if termWeeks <= 0 {
		return 0, ErrInvalidTerm
	}
	if totalPayable <= 0 {
		return 0, ErrAmountNotPositive
	}
	return totalPayable / Kobo(termWeeks), nil
}

// FinalInstalment is the last week's instalment, absorbing any rounding
// remainder so that the instalments sum exactly to the obligation.
func FinalInstalment(totalPayable Kobo, termWeeks int) (Kobo, error) {
	weekly, err := WeeklyDue(totalPayable, termWeeks)
	if err != nil {
		return 0, err
	}
	return totalPayable - weekly*Kobo(termWeeks-1), nil
}

// Outstanding is the amount still owed on the obligation.
func (a Account) Outstanding() Kobo {
	return a.TotalPayable - a.TotalPaid
}

// Allocation is the result of applying a payment to an account. It is a pure
// value: computing it changes nothing, which is what makes it cheap to test
// exhaustively and safe to reason about.
type Allocation struct {
	// Applied reduced the outstanding obligation.
	Applied Kobo
	// Excess exceeded the obligation and belongs in the credit bucket. It is
	// never silently discarded and never drives the balance negative.
	Excess Kobo
	// Settles reports whether this payment completed the deployment.
	Settles bool

	NewTotalPaid    Kobo
	NewOverpayment  Kobo
	NewOutstanding  Kobo
	NewStatus       Status
	InstalmentsMet  int
	PartPaidCurrent Kobo
}

// Allocate applies an amount to the account under FIFO: the payment settles the
// oldest unpaid instalment first, then the next, and so on.
//
// Because instalments are uniform and interest-inclusive, "oldest first" needs
// no per-instalment table and no allocation loop -- the settled count is integer
// division on the running total. Materialising a 50-row schedule per deployment
// and mutating it per payment would create a second mutable source of truth that
// can drift from the ledger, which is the exact failure this design exists to
// prevent.
func (a Account) Allocate(amount Kobo) (Allocation, error) {
	if amount <= 0 {
		return Allocation{}, ErrAmountNotPositive
	}
	if a.Status != StatusActive {
		return Allocation{}, ErrAccountNotActive
	}

	outstanding := a.Outstanding()

	applied := amount
	var excess Kobo
	if applied > outstanding {
		applied, excess = outstanding, amount-outstanding
	}

	alloc := Allocation{
		Applied:        applied,
		Excess:         excess,
		NewTotalPaid:   a.TotalPaid + applied,
		NewOverpayment: a.Overpayment + excess,
		NewStatus:      StatusActive,
	}
	alloc.NewOutstanding = a.TotalPayable - alloc.NewTotalPaid
	alloc.Settles = alloc.NewOutstanding == 0
	if alloc.Settles {
		alloc.NewStatus = StatusCompleted
	}

	alloc.InstalmentsMet, alloc.PartPaidCurrent = instalmentsMet(
		alloc.NewTotalPaid, a.TotalPayable, a.WeeklyDue, a.TermWeeks)

	return alloc, nil
}

// Position is the customer's current standing: not just what they owe, but
// whether they are keeping pace. The balance alone cannot distinguish a customer
// who is four weeks ahead from one who is four weeks in arrears, and only the
// second needs collections.
type Position struct {
	AccountID    uuid.UUID
	CustomerID   string
	Status       Status
	TotalPayable Kobo
	TotalPaid    Kobo
	Outstanding  Kobo
	Overpayment  Kobo
	WeeklyDue    Kobo
	TermWeeks    int
	StartDate    time.Time

	// WeeksElapsed counts completed weeks since deployment.
	WeeksElapsed int
	// InstalmentsMet is how many full instalments the payments cover (FIFO).
	InstalmentsMet int
	// PartPaidCurrent is payment sitting against the next, part-filled instalment.
	PartPaidCurrent Kobo
	// NextDueWeek is the instalment number now being collected, or 0 if none remain.
	NextDueWeek int

	// ExpectedToDate is what should have been paid by now under the schedule.
	ExpectedToDate Kobo
	// Arrears is the shortfall against ExpectedToDate; zero if on or ahead of schedule.
	Arrears Kobo
	// AheadBy is prepayment beyond ExpectedToDate; zero if behind.
	AheadBy Kobo
	// WeeksBehind rounds arrears up to whole weeks: owing any part of a week counts as behind.
	WeeksBehind int

	ScheduledCompletion time.Time
}

// PositionAt derives the customer's standing as of now.
//
// now is a parameter rather than a call to time.Now inside the function: a
// position that depends on a hidden clock cannot be tested at a week boundary,
// and week boundaries are exactly where delinquency logic goes wrong.
func (a Account) PositionAt(now time.Time) Position {
	p := Position{
		AccountID:           a.ID,
		CustomerID:          a.CustomerID,
		Status:              a.Status,
		TotalPayable:        a.TotalPayable,
		TotalPaid:           a.TotalPaid,
		Outstanding:         a.Outstanding(),
		Overpayment:         a.Overpayment,
		WeeklyDue:           a.WeeklyDue,
		TermWeeks:           a.TermWeeks,
		StartDate:           a.StartDate,
		ScheduledCompletion: a.StartDate.AddDate(0, 0, a.TermWeeks*DaysPerWeek),
	}

	p.WeeksElapsed = weeksBetween(a.StartDate, now)

	instalmentsDue := min(p.WeeksElapsed, a.TermWeeks)
	if instalmentsDue >= a.TermWeeks {
		// The final instalment carries the rounding remainder, so the whole
		// obligation is due once the term has run.
		p.ExpectedToDate = a.TotalPayable
	} else {
		p.ExpectedToDate = a.WeeklyDue * Kobo(instalmentsDue)
	}

	if shortfall := p.ExpectedToDate - a.TotalPaid; shortfall > 0 {
		p.Arrears = shortfall
		if a.WeeklyDue > 0 {
			// Ceiling division: part of a week overdue is still behind.
			p.WeeksBehind = int((shortfall + a.WeeklyDue - 1) / a.WeeklyDue)
		}
	} else {
		p.AheadBy = -shortfall
	}

	p.InstalmentsMet, p.PartPaidCurrent = instalmentsMet(
		a.TotalPaid, a.TotalPayable, a.WeeklyDue, a.TermWeeks)
	if p.InstalmentsMet < a.TermWeeks {
		p.NextDueWeek = p.InstalmentsMet + 1
	}

	return p
}

// instalmentsMet is the FIFO reduction: how many whole instalments the running
// total covers, and what is left over against the next one.
func instalmentsMet(totalPaid, totalPayable, weeklyDue Kobo, termWeeks int) (met int, partPaid Kobo) {
	if weeklyDue <= 0 {
		return 0, 0
	}
	if totalPaid >= totalPayable {
		return termWeeks, 0
	}
	met = int(totalPaid / weeklyDue)
	if met >= termWeeks {
		// Reachable when the final instalment carries a remainder: the earlier
		// weeks are all settled but the obligation is not yet discharged.
		return termWeeks - 1, totalPaid - weeklyDue*Kobo(termWeeks-1)
	}
	return met, totalPaid - weeklyDue*Kobo(met)
}

// weeksBetween counts whole 7-day periods elapsed, measured on calendar dates so
// that a payment at 23:59 and one at 00:01 the next minute do not land in
// different weeks because of a clock reading rather than a real boundary.
func weeksBetween(start, now time.Time) int {
	s := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	n := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	days := int(n.Sub(s).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days / DaysPerWeek
}
