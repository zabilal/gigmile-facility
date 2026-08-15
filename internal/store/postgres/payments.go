package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/zabilal/gigmile-facility/internal/domain"
)

// Outcome is what happened to a notification.
type Outcome string

const (
	// OutcomeApplied moved money: a ledger entry exists and the balance changed.
	OutcomeApplied Outcome = "applied"
	// OutcomeDuplicate means the reference was already processed. The response
	// mirrors the original so a retrying provider converges instead of looping.
	OutcomeDuplicate Outcome = "duplicate"
	// OutcomeIgnored recorded a non-settling notification, untouched ledger.
	OutcomeIgnored Outcome = "ignored"
	// OutcomeSuspense recorded it but could not safely apply it. Needs ops.
	OutcomeSuspense Outcome = "suspense"
)

// ErrAmountMismatch means a known reference arrived again with a different
// amount: a provider defect, or someone probing whether a bigger number sticks.
var ErrAmountMismatch = errors.New("transaction_reference already recorded with a different amount")

// Result describes the disposition of one notification.
type Result struct {
	Outcome   Outcome
	Reference string
	PaymentID int64
	// Reason is set for ignored and suspense outcomes, so ops can triage
	// without re-deriving why.
	Reason   string
	Applied  domain.Kobo
	Excess   domain.Kobo
	Position *domain.Position
}

const insertPaymentSQL = `
INSERT INTO payments (transaction_reference, customer_id, amount_kobo, payment_status,
                      transaction_at, received_at, state, raw)
VALUES ($1, $2, $3, $4, $5, $6, 'PENDING', $7)
ON CONFLICT (transaction_reference) DO NOTHING
RETURNING id`

// applyPaymentSQL settles a payment in one statement: prev takes the row lock and
// captures the pre-image, since RETURNING alone cannot see the old values.
const applyPaymentSQL = `
WITH prev AS (
    SELECT id, total_paid_kobo, total_payable_kobo, overpayment_kobo
      FROM loan_accounts
     WHERE customer_id = $1
       AND status = 'ACTIVE'
       AND total_paid_kobo < total_payable_kobo
       FOR UPDATE
)
UPDATE loan_accounts a
   SET total_paid_kobo  = LEAST(prev.total_paid_kobo + $2, prev.total_payable_kobo),
       overpayment_kobo = prev.overpayment_kobo
                        + GREATEST(prev.total_paid_kobo + $2 - prev.total_payable_kobo, 0),
       status           = CASE WHEN prev.total_paid_kobo + $2 >= prev.total_payable_kobo
                               THEN 'COMPLETED' ELSE a.status END,
       version          = a.version + 1,
       updated_at       = now()
  FROM prev
 WHERE a.id = prev.id
RETURNING a.id, prev.total_paid_kobo, a.total_paid_kobo, a.overpayment_kobo,
          a.total_payable_kobo, a.asset_value_kobo, a.weekly_due_kobo,
          a.term_weeks, a.start_date, a.status, a.version`

const insertLedgerEntrySQL = `
INSERT INTO ledger_entries (account_id, payment_id, entry_type, amount_kobo, balance_after_kobo)
VALUES ($1, $2, 'REPAYMENT', $3, $4)`

const finalisePaymentSQL = `
UPDATE payments SET state = $2, account_id = $3, outcome_reason = $4 WHERE id = $1`

// ApplyPayment records a notification and applies it when it settles funds.
// Record, ledger entry and balance move in one transaction or not at all.
func (s *Store) ApplyPayment(ctx context.Context, n domain.Notification, raw []byte, now time.Time) (Result, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var paymentID int64
	err = tx.QueryRow(ctx, insertPaymentSQL,
		n.Reference, n.CustomerID, int64(n.Amount), string(n.Status), n.TransactionAt, now, raw,
	).Scan(&paymentID)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The unique index refused the insert. Concurrent duplicates block here
		// until the first commits, so both converge instead of both applying.
		res, err := s.describeExisting(ctx, tx, n, now)
		if err != nil {
			return Result{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Result{}, fmt.Errorf("commit duplicate: %w", err)
		}
		return res, nil
	case err != nil:
		return Result{}, fmt.Errorf("record payment: %w", err)
	}

	result := Result{Reference: n.Reference, PaymentID: paymentID}

	switch d := n.Disposition(); d {
	case domain.DispositionIgnore:
		result.Outcome = OutcomeIgnored
		result.Reason = fmt.Sprintf("payment_status %q does not settle funds", n.Status)
		if err := finalise(ctx, tx, paymentID, "IGNORED", uuid.Nil, result.Reason); err != nil {
			return Result{}, err
		}
	case domain.DispositionReview:
		result.Outcome = OutcomeSuspense
		result.Reason = fmt.Sprintf("payment_status %q needs manual review", n.Status)
		if err := finalise(ctx, tx, paymentID, "SUSPENSE", uuid.Nil, result.Reason); err != nil {
			return Result{}, err
		}
	case domain.DispositionApply:
		if err := s.settle(ctx, tx, n, paymentID, now, &result); err != nil {
			return Result{}, err
		}
	default:
		return Result{}, fmt.Errorf("unhandled disposition %q", d)
	}

	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit: %w", err)
	}
	return result, nil
}

// settle applies a COMPLETE payment to the customer's active deployment.
func (s *Store) settle(ctx context.Context, tx pgx.Tx, n domain.Notification, paymentID int64, now time.Time, result *Result) error {
	var (
		acct         domain.Account
		previousPaid int64
		status       string
	)

	err := tx.QueryRow(ctx, applyPaymentSQL, n.CustomerID, int64(n.Amount)).Scan(
		&acct.ID, &previousPaid, &acct.TotalPaid, &acct.Overpayment,
		&acct.TotalPayable, &acct.AssetValue, &acct.WeeklyDue,
		&acct.TermWeeks, &acct.StartDate, &status, &acct.Version,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// Unknown customer, or one completed and not yet redeployed. The money
		// arrived and is not ours to discard, so record it for reconciliation.
		result.Outcome = OutcomeSuspense
		result.Reason = "no active deployment for customer"
		return finalise(ctx, tx, paymentID, "SUSPENSE", uuid.Nil, result.Reason)
	}
	if err != nil {
		return fmt.Errorf("apply to account: %w", err)
	}

	acct.CustomerID = n.CustomerID
	acct.Status = domain.Status(status)

	applied := acct.TotalPaid - domain.Kobo(previousPaid)
	outstanding := acct.TotalPayable - acct.TotalPaid

	if _, err := tx.Exec(ctx, insertLedgerEntrySQL, acct.ID, paymentID, int64(applied), int64(outstanding)); err != nil {
		return fmt.Errorf("post ledger entry: %w", err)
	}
	if err := finalise(ctx, tx, paymentID, "APPLIED", acct.ID, ""); err != nil {
		return err
	}

	position := acct.PositionAt(now)
	result.Outcome = OutcomeApplied
	result.Applied = applied
	result.Excess = n.Amount - applied
	result.Position = &position
	return nil
}

const describeExistingSQL = `
SELECT id, amount_kobo, state, account_id, outcome_reason
  FROM payments
 WHERE transaction_reference = $1`

// describeExisting reconstructs a known reference's outcome, so a retry gets the
// same answer as the original -- which is what makes a provider stop retrying.
func (s *Store) describeExisting(ctx context.Context, tx pgx.Tx, n domain.Notification, now time.Time) (Result, error) {
	var (
		paymentID int64
		amount    int64
		state     string
		accountID *uuid.UUID
		reason    *string
	)

	if err := tx.QueryRow(ctx, describeExistingSQL, n.Reference).Scan(
		&paymentID, &amount, &state, &accountID, &reason,
	); err != nil {
		return Result{}, fmt.Errorf("load existing payment: %w", err)
	}

	if domain.Kobo(amount) != n.Amount {
		return Result{}, fmt.Errorf("%w: on file %s, received %s",
			ErrAmountMismatch, domain.Kobo(amount), n.Amount)
	}

	result := Result{
		Outcome:   OutcomeDuplicate,
		Reference: n.Reference,
		PaymentID: paymentID,
	}
	if reason != nil {
		result.Reason = *reason
	}
	if result.Reason == "" {
		result.Reason = fmt.Sprintf("already processed (%s)", state)
	}

	if accountID != nil {
		acct, err := loadAccountByID(ctx, tx, *accountID)
		if err != nil {
			return Result{}, err
		}
		position := acct.PositionAt(now)
		result.Position = &position
	}
	return result, nil
}

func finalise(ctx context.Context, tx pgx.Tx, paymentID int64, state string, accountID uuid.UUID, reason string) error {
	var acct *uuid.UUID
	if accountID != uuid.Nil {
		acct = &accountID
	}
	var why *string
	if reason != "" {
		why = &reason
	}
	if _, err := tx.Exec(ctx, finalisePaymentSQL, paymentID, state, acct, why); err != nil {
		return fmt.Errorf("finalise payment as %s: %w", state, err)
	}
	return nil
}
