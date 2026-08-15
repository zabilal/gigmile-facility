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

// ErrAlreadyDeployed means the customer already holds an active deployment.
// Surfaced from the partial unique index, so it holds under concurrency.
var ErrAlreadyDeployed = errors.New("customer already has an active deployment")

// Deployment describes an asset handed to a customer.
type Deployment struct {
	CustomerID   string
	AssetValue   domain.Kobo
	TotalPayable domain.Kobo
	TermWeeks    int
	StartDate    time.Time
	// PriorPaid is repayment made before this record existed. Posted as an
	// ADJUSTMENT entry, never written straight to the balance.
	PriorPaid domain.Kobo
}

const insertDeploymentSQL = `
INSERT INTO loan_accounts (id, customer_id, asset_value_kobo, total_payable_kobo, weekly_due_kobo,
                           total_paid_kobo, term_weeks, start_date, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'ACTIVE')
RETURNING ` + accountColumns

const insertOpeningBalanceSQL = `
INSERT INTO ledger_entries (account_id, payment_id, entry_type, amount_kobo, balance_after_kobo)
VALUES ($1, NULL, 'ADJUSTMENT', $2, $3)`

// Deploy creates an active deployment. Origination proper belongs to another
// service; this exists so the seeder and tests can create state, and is unexposed.
func (s *Store) Deploy(ctx context.Context, d Deployment) (domain.Account, error) {
	weekly, err := domain.WeeklyDue(d.TotalPayable, d.TermWeeks)
	if err != nil {
		return domain.Account{}, err
	}
	if d.PriorPaid < 0 || d.PriorPaid >= d.TotalPayable {
		return domain.Account{}, fmt.Errorf("prior paid %s must be within [0, %s)", d.PriorPaid, d.TotalPayable)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Account{}, fmt.Errorf("begin deployment: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	acct, err := scanAccount(tx.QueryRow(ctx, insertDeploymentSQL,
		uuid.New(), d.CustomerID, int64(d.AssetValue), int64(d.TotalPayable),
		int64(weekly), int64(d.PriorPaid), d.TermWeeks, d.StartDate))
	if isUniqueViolation(err, "one_active_deployment_per_customer") {
		return domain.Account{}, ErrAlreadyDeployed
	}
	if err != nil {
		return domain.Account{}, fmt.Errorf("create deployment: %w", err)
	}

	// Through the ledger like everything else: a balance with no entry
	// explaining it would stop being derivable from the ledger.
	if d.PriorPaid > 0 {
		if _, err := tx.Exec(ctx, insertOpeningBalanceSQL,
			acct.ID, int64(d.PriorPaid), int64(d.TotalPayable-d.PriorPaid)); err != nil {
			return domain.Account{}, fmt.Errorf("post opening balance: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.Account{}, fmt.Errorf("commit deployment: %w", err)
	}
	return acct, nil
}

// BulkDeploy inserts over COPY, since seeding needs ~100k accounts. Accounts and
// opening entries copy in one transaction, so balances never outlive their ledger.
func (s *Store) BulkDeploy(ctx context.Context, deployments []Deployment) (int64, error) {
	accountRows := make([][]any, 0, len(deployments))
	ledgerRows := make([][]any, 0, len(deployments))

	for _, d := range deployments {
		weekly, err := domain.WeeklyDue(d.TotalPayable, d.TermWeeks)
		if err != nil {
			return 0, fmt.Errorf("customer %s: %w", d.CustomerID, err)
		}
		if d.PriorPaid < 0 || d.PriorPaid >= d.TotalPayable {
			return 0, fmt.Errorf("customer %s: prior paid %s must be within [0, %s)",
				d.CustomerID, d.PriorPaid, d.TotalPayable)
		}

		id := uuid.New()
		accountRows = append(accountRows, []any{
			id, d.CustomerID, int64(d.AssetValue), int64(d.TotalPayable),
			int64(weekly), int64(d.PriorPaid), d.TermWeeks, d.StartDate, "ACTIVE",
		})
		if d.PriorPaid > 0 {
			ledgerRows = append(ledgerRows, []any{
				id, nil, "ADJUSTMENT", int64(d.PriorPaid), int64(d.TotalPayable - d.PriorPaid),
			})
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin bulk deploy: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	n, err := tx.CopyFrom(ctx,
		pgx.Identifier{"loan_accounts"},
		[]string{"id", "customer_id", "asset_value_kobo", "total_payable_kobo",
			"weekly_due_kobo", "total_paid_kobo", "term_weeks", "start_date", "status"},
		pgx.CopyFromRows(accountRows))
	if err != nil {
		return 0, fmt.Errorf("bulk deploy accounts: %w", err)
	}

	if len(ledgerRows) > 0 {
		if _, err := tx.CopyFrom(ctx,
			pgx.Identifier{"ledger_entries"},
			[]string{"account_id", "payment_id", "entry_type", "amount_kobo", "balance_after_kobo"},
			pgx.CopyFromRows(ledgerRows)); err != nil {
			return 0, fmt.Errorf("bulk deploy opening balances: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit bulk deploy: %w", err)
	}
	return n, nil
}
