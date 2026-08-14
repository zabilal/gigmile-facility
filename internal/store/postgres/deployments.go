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
//
// Surfaced from the database's partial unique index rather than from a prior
// existence check, so it holds under concurrency: two simultaneous deployments
// for one customer cannot both win a race that the index refuses to allow.
var ErrAlreadyDeployed = errors.New("customer already has an active deployment")

// Deployment describes an asset handed to a customer.
type Deployment struct {
	CustomerID   string
	AssetValue   domain.Kobo
	TotalPayable domain.Kobo
	TermWeeks    int
	StartDate    time.Time
	// PriorPaid carries repayment already made before this record existed --
	// a deployment onboarded from another system, or a seeded book. It is posted
	// as an ADJUSTMENT ledger entry rather than written straight to the balance,
	// because a balance that is not the sum of its ledger is exactly the drift
	// this design exists to prevent.
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

// Deploy creates an active deployment for a customer.
//
// Origination proper -- credit decisioning, asset allocation, contract terms --
// belongs to another service. This exists so the seeder and the tests can create
// the state the payment path operates on, and it is not exposed over HTTP.
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

	// The opening balance goes through the ledger like everything else. Writing
	// it straight to total_paid_kobo would create a balance with no entry
	// explaining it -- the projection would no longer be derivable from the
	// ledger, which is the one invariant this design is built around.
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

// BulkDeploy inserts deployments using the Postgres COPY protocol.
//
// COPY rather than a loop of INSERTs because seeding the load test needs ~100k
// accounts: a per-row round trip makes that minutes of waiting, while COPY
// streams it in seconds. The seeded cardinality is what makes the load test
// meaningful -- fire 100k payments/minute at ten accounts and the benchmark
// measures row-lock contention rather than the system.
//
// Accounts and their opening-balance entries are copied in one transaction, so a
// failed seed cannot leave balances without the ledger that justifies them.
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
