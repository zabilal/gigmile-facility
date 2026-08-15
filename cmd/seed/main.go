// Command seed populates the deployment book, standing in for the origination
// service. Load-test infrastructure: ten accounts would measure only contention.
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
		// -reset would destroy a real ledger, so refuse anywhere but local
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

			// Stagger across the term so positions vary: a book deployed all
			// today makes every position identical and hides arrears bugs.
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

// headroomWeeks keeps seeded accounts short of completion so a load test measures
// steady-state writes. Two weeks is ample: a run spreads ~2 payments per account.
const headroomWeeks = 2

// seededPaid spreads repayment health realistically: mostly on schedule, some
// in arrears, some prepaid. Deterministic, so a seeded database is reproducible.
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
