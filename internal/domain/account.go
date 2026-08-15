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
	// StatusCompleted means the obligation is repaid and the asset is owned.
	StatusCompleted Status = "COMPLETED"
	// StatusWrittenOff means collection was abandoned; payments no longer apply.
	StatusWrittenOff Status = "WRITTEN_OFF"
)

// DaysPerWeek is the repayment cadence, named because the week is a business
// unit here rather than an arbitrary 7.
const DaysPerWeek = 7

var (
	// ErrAccountNotActive means the deployment cannot accept repayments.
	ErrAccountNotActive = errors.New("account is not active")
	// ErrInvalidTerm means the term was not a positive number of weeks.
	ErrInvalidTerm = errors.New("term must be a positive number of weeks")
)

// Account is the materialised position of a single asset deployment. The
// one-active-deployment rule is enforced by the database, not here.
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

// WeeklyDue computes the uniform instalment, flooring so the remainder can be
// carried to the final week rather than charging fractions of a kobo.
func WeeklyDue(totalPayable Kobo, termWeeks int) (Kobo, error) {
	if termWeeks <= 0 {
		return 0, ErrInvalidTerm
	}
	if totalPayable <= 0 {
		return 0, ErrAmountNotPositive
	}
	return totalPayable / Kobo(termWeeks), nil
}

// FinalInstalment is the last week's instalment, absorbing the rounding
// remainder so the instalments sum exactly to the obligation.
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

// Allocation is the result of applying a payment. A pure value: computing it
// changes nothing, which is what makes it cheap to test exhaustively.
type Allocation struct {
	// Applied reduced the outstanding obligation.
	Applied Kobo
	// Excess exceeded the obligation and belongs in the credit bucket. Never
	// discarded, and never drives the balance negative.
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

// Allocate applies an amount under FIFO: oldest unpaid instalment first. With
// uniform instalments that needs no schedule table -- see instalmentsMet.
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

// Position is the customer's standing, not just their balance: a balance alone
// cannot tell someone four weeks ahead from four weeks in arrears.
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
	// PartPaidCurrent sits against the next, part-filled instalment.
	PartPaidCurrent Kobo
	// NextDueWeek is the instalment now being collected, or 0 if none remain.
	NextDueWeek int

	// ExpectedToDate is what the schedule says should have been paid by now.
	ExpectedToDate Kobo
	// Arrears is the shortfall; zero if on or ahead of schedule.
	Arrears Kobo
	// AheadBy is prepayment beyond ExpectedToDate; zero if behind.
	AheadBy Kobo
	// WeeksBehind rounds arrears up: owing part of a week counts as behind.
	WeeksBehind int

	ScheduledCompletion time.Time
}

// PositionAt derives the customer's standing as of now. now is a parameter so
// this is testable at a week boundary, which is where delinquency logic breaks.
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
		// The final instalment carries the remainder, so once the term has run
		// the whole obligation is due.
		p.ExpectedToDate = a.TotalPayable
	} else {
		p.ExpectedToDate = a.WeeklyDue * Kobo(instalmentsDue)
	}

	if shortfall := p.ExpectedToDate - a.TotalPaid; shortfall > 0 {
		p.Arrears = shortfall
		if a.WeeklyDue > 0 {
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

// instalmentsMet is the FIFO reduction: uniform instalments make "oldest first"
// integer division, so no per-instalment table can drift from the ledger.
func instalmentsMet(totalPaid, totalPayable, weeklyDue Kobo, termWeeks int) (met int, partPaid Kobo) {
	if weeklyDue <= 0 {
		return 0, 0
	}
	if totalPaid >= totalPayable {
		return termWeeks, 0
	}
	met = int(totalPaid / weeklyDue)
	if met >= termWeeks {
		// The final instalment carries a remainder: earlier weeks are settled
		// but the obligation is not yet discharged.
		return termWeeks - 1, totalPaid - weeklyDue*Kobo(termWeeks-1)
	}
	return met, totalPaid - weeklyDue*Kobo(met)
}

// weeksBetween counts whole 7-day periods on calendar dates, so a payment at
// 23:59 and one a minute later do not land in different weeks.
func weeksBetween(start, now time.Time) int {
	s := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	n := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	days := int(n.Sub(s).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days / DaysPerWeek
}
