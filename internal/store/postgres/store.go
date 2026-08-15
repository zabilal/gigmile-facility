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

// Store is the persistence adapter. Business rules live in the domain package;
// this one holds only the statements that move them to disk.
type Store struct {
	pool *pgxpool.Pool
}

// New opens a connection pool and verifies it can reach the database.
func New(ctx context.Context, databaseURL string, maxConns int32) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	// Sized for the machine, not the request rate: past what the server can run
	// concurrently, more connections add contention rather than throughput.
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

// Ping backs the readiness probe: an instance that cannot reach Postgres should
// leave the load balancer, but not be restarted -- so liveness does not call it.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Pool exposes the underlying pool for the seeder's bulk-copy path.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Reset clears the book; development only. TRUNCATE not DELETE because the
// immutability trigger is row-level: a wipe differs from rewriting history.
func (s *Store) Reset(ctx context.Context) error {
	_, err := s.pool.Exec(ctx,
		`TRUNCATE ledger_entries, payments, loan_accounts RESTART IDENTITY CASCADE`)
	if err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	return nil
}

// isUniqueViolation reports a unique-constraint error. Idempotency is decided by
// the database refusing a second insert, never by a check that races the write.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23505" && (constraint == "" || pgErr.ConstraintName == constraint)
}
