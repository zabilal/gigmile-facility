// Package postgres persists payments, the ledger, and account positions.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the persistence adapter. The domain package holds the business
// rules; this package holds only the statements that move them to disk.
type Store struct {
	pool *pgxpool.Pool
}

// New opens a connection pool and verifies it can reach the database.
func New(ctx context.Context, databaseURL string, maxConns int32) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	// Pool size is a property of the database and the machine, not of the
	// request rate. Past the point where connections exceed what the server can
	// run concurrently, more of them reduce throughput: the work is the same but
	// contention on it grows. Queueing in the pool is the correct back pressure.
	cfg.MaxConns = maxConns
	cfg.MinConns = 2
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping reports whether the database is reachable. Used by the readiness probe:
// an instance that cannot reach Postgres should be taken out of the load
// balancer, but not restarted, which is why liveness does not call this.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Pool exposes the underlying pool for the seeder's bulk-copy path.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Reset clears the book. Development helper for the seeder only; callers must
// gate it on the environment.
//
// TRUNCATE rather than DELETE is not just for speed: the ledger's immutability
// trigger is row-level and fires on DELETE, so erasing history transactionally
// is impossible by design. A deliberate administrative truncate is a different
// act from a transaction quietly rewriting the past, and only the second is
// what the trigger exists to stop.
func (s *Store) Reset(ctx context.Context) error {
	_, err := s.pool.Exec(ctx,
		`TRUNCATE ledger_entries, payments, loan_accounts RESTART IDENTITY CASCADE`)
	if err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	return nil
}

// isUniqueViolation reports whether err is a Postgres unique-constraint error
// on the named constraint. Idempotency is decided by the database rejecting a
// second insert, never by an application-level "does this exist?" check, which
// races between the read and the write.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23505" && (constraint == "" || pgErr.ConstraintName == constraint)
}
