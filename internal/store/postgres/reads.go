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

// ErrAccountNotFound means the customer has no deployment on record.
var ErrAccountNotFound = errors.New("no deployment found for customer")

// querier is satisfied by both the pool and a transaction, so reads work inside
// an in-flight write or standalone without duplicating the SQL.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

const accountColumns = `id, customer_id, asset_value_kobo, total_payable_kobo, weekly_due_kobo,
       total_paid_kobo, overpayment_kobo, term_weeks, start_date, status, version`

func scanAccount(row pgx.Row) (domain.Account, error) {
	var (
		a      domain.Account
		status string
	)
	err := row.Scan(&a.ID, &a.CustomerID, &a.AssetValue, &a.TotalPayable, &a.WeeklyDue,
		&a.TotalPaid, &a.Overpayment, &a.TermWeeks, &a.StartDate, &status, &a.Version)
	if err != nil {
		return domain.Account{}, err
	}
	a.Status = domain.Status(status)
	return a, nil
}

func loadAccountByID(ctx context.Context, q querier, id uuid.UUID) (domain.Account, error) {
	acct, err := scanAccount(q.QueryRow(ctx, `SELECT `+accountColumns+` FROM loan_accounts WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Account{}, ErrAccountNotFound
	}
	if err != nil {
		return domain.Account{}, fmt.Errorf("load account: %w", err)
	}
	return acct, nil
}

// currentAccountSQL prefers the active deployment, falling back to the most
// recent, so a customer who has finished repaying still gets a position.
const currentAccountSQL = `
SELECT ` + accountColumns + `
  FROM loan_accounts
 WHERE customer_id = $1
 ORDER BY (status = 'ACTIVE') DESC, created_at DESC
 LIMIT 1`

// Position returns the customer's current standing.
func (s *Store) Position(ctx context.Context, customerID string, now time.Time) (domain.Position, error) {
	acct, err := scanAccount(s.pool.QueryRow(ctx, currentAccountSQL, customerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Position{}, ErrAccountNotFound
	}
	if err != nil {
		return domain.Position{}, fmt.Errorf("load position: %w", err)
	}
	return acct.PositionAt(now), nil
}

// LedgerEntry is one immutable line of a customer's statement.
type LedgerEntry struct {
	ID           int64
	EntryType    string
	Reference    string
	Amount       domain.Kobo
	BalanceAfter domain.Kobo
	CreatedAt    time.Time
}

// ledgerSQL pages by keyset, not OFFSET. LEFT JOIN because opening balances have
// no payment, and an inner join would drop them out of the statement.
const ledgerSQL = `
SELECT e.id, e.entry_type, COALESCE(p.transaction_reference, ''),
       e.amount_kobo, e.balance_after_kobo, e.created_at
  FROM ledger_entries e
  LEFT JOIN payments p ON p.id = e.payment_id
 WHERE e.account_id = $1
   AND ($2::BIGINT IS NULL OR e.id < $2)
 ORDER BY e.id DESC
 LIMIT $3`

// Ledger returns a page of the statement, newest first. before is the cursor:
// nil for the first page, then the last ID of the previous one.
func (s *Store) Ledger(ctx context.Context, customerID string, before *int64, limit int) ([]LedgerEntry, error) {
	acct, err := scanAccount(s.pool.QueryRow(ctx, currentAccountSQL, customerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("resolve account: %w", err)
	}

	rows, err := s.pool.Query(ctx, ledgerSQL, acct.ID, before, limit)
	if err != nil {
		return nil, fmt.Errorf("query ledger: %w", err)
	}
	defer rows.Close()

	entries := make([]LedgerEntry, 0, limit)
	for rows.Next() {
		var e LedgerEntry
		if err := rows.Scan(&e.ID, &e.EntryType, &e.Reference, &e.Amount, &e.BalanceAfter, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan ledger entry: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ReconcileAccount makes the ledger-is-truth invariant executable: it recomputes
// the paid total from entries and compares. Drift means a bug that cost money.
func (s *Store) ReconcileAccount(ctx context.Context, accountID uuid.UUID) (ledgerTotal, materialised domain.Kobo, err error) {
	const sql = `
SELECT COALESCE((SELECT SUM(amount_kobo) FROM ledger_entries WHERE account_id = $1), 0),
       (SELECT total_paid_kobo FROM loan_accounts WHERE id = $1)`

	if err := s.pool.QueryRow(ctx, sql, accountID).Scan(&ledgerTotal, &materialised); err != nil {
		return 0, 0, fmt.Errorf("reconcile: %w", err)
	}
	return ledgerTotal, materialised, nil
}
