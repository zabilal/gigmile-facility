// Command seed populates the deployment book.
//
// The brief assumes customers and deployments already exist; on a fresh clone
// they do not. Origination belongs to another bounded context, so rather than
// building management endpoints this service has no business owning, a seeder
// stands in for that upstream.
//
// It is also load-test infrastructure, not a convenience. The design claims
// contention is negligible because payments spread across many customers. Fire
// 100k payments a minute at ten accounts and the benchmark measures row-lock
// contention rather than the system, so the seeded cardinality is what makes the
// throughput number mean anything.
//
//	go run ./cmd/seed -count=100000 -reset
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/zabilal/gigmile-facility/internal/domain"
	"github.com/zabilal/gigmile-facility/internal/platform/config"
	"github.com/zabilal/gigmile-facility/internal/store/postgres"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("seed: %v", err)
	}
}

func run() error {
	var (
		count   = flag.Int("count", 100_000, "number of deployments to create")
		batch   = flag.Int("batch", 10_000, "rows per COPY batch")
		reset   = flag.Bool("reset", false, "delete all existing data first")
		payable = flag.Int64("payable", 1_000_000, "amount repayable per deployment, in naira")
		term    = flag.Int("term", 50, "term in weeks")
	)
	flag.Parse()

	if *count <= 0 || *batch <= 0 {
		return fmt.Errorf("count and batch must be positive")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx := context.Background()
	store, err := postgres.New(ctx, cfg.DatabaseURL, cfg.MaxPoolConns)
	if err != nil {
		return err
	}
	defer store.Close()

	if *reset {
		// -reset against a real environment would destroy the ledger. The flag
		// is only meaningful for a local database, so refuse anywhere else
		// rather than trusting whoever typed the command.
		if cfg.Environment != "development" {
			return fmt.Errorf("-reset refused in environment %q", cfg.Environment)
		}
		if err := store.Reset(ctx); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "reset: existing book cleared")
	}

	totalPayable := domain.Kobo(*payable) * domain.KobosPerNaira
	weekly, err := domain.WeeklyDue(totalPayable, *term)
	if err != nil {
		return err
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)

	start := time.Now()
	var seeded int64

	for offset := 0; offset < *count; offset += *batch {
		size := min(*batch, *count-offset)
		deployments := make([]postgres.Deployment, 0, size)

		for i := offset; i < offset+size; i++ {
			d := postgres.Deployment{
				CustomerID:   fmt.Sprintf("GIG%06d", i+1),
				AssetValue:   totalPayable * 8 / 10, // the margin is baked into total payable
				TotalPayable: totalPayable,
				TermWeeks:    *term,
			}

			// Stagger the book across the term so positions vary. A book where
			// every account was deployed today makes every position identical
			// and hides every bug in the arrears arithmetic.
			weeksIn := i % *term
			d.StartDate = today.AddDate(0, 0, -weeksIn*domain.DaysPerWeek)
			d.PriorPaid = seededPaid(i, weekly, weeksIn, totalPayable)

			deployments = append(deployments, d)
		}

		n, err := store.BulkDeploy(ctx, deployments)
		if err != nil {
			return err
		}
		seeded += n

		fmt.Fprintf(os.Stderr, "\rseeded %d/%d", seeded, *count)
	}

	elapsed := time.Since(start)
	fmt.Fprintf(os.Stderr, "\rseeded %d deployments in %s (%.0f rows/sec)\n",
		seeded, elapsed.Round(time.Millisecond), float64(seeded)/elapsed.Seconds())
	fmt.Fprintf(os.Stderr, "customer ids: GIG%06d .. GIG%06d\n", 1, *count)

	return nil
}

// headroomWeeks is how much of the obligation is left unpaid on every seeded
// account, so a load test does not start completing them and drifting into the
// suspense path -- which would measure something other than the steady-state
// write cost.
//
// Two weeks is ample: a one-minute run at 100k payments/minute spreads roughly
// two payments over each of 100k accounts. An earlier draft reserved half the
// obligation, which was not conservative but wrong -- it clipped every account
// past week 25, so the on-schedule cohort rendered as delinquent and the seeded
// book misrepresented the very spread it exists to provide.
const headroomWeeks = 2

// seededPaid gives the book a realistic spread of repayment health: mostly on
// schedule, some in arrears, some prepaid, some deployed but not yet paying.
// Deterministic rather than random, so a seeded database is reproducible.
func seededPaid(i int, weekly domain.Kobo, weeksIn int, totalPayable domain.Kobo) domain.Kobo {
	onSchedule := weekly * domain.Kobo(weeksIn)

	var paid domain.Kobo
	switch i % 10 {
	case 6, 7: // 20% in arrears
		paid = onSchedule * 3 / 5
	case 8: // 10% running ahead
		paid = onSchedule + 5*weekly
	case 9: // 10% deployed, never paid -- the delinquency the collections team cares about
		paid = 0
	default: // 60% on schedule
		paid = onSchedule
	}

	if ceiling := totalPayable - headroomWeeks*weekly; paid > ceiling {
		paid = ceiling
	}
	return paid
}
