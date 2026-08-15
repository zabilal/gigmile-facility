// Command migrate applies or rolls back database migrations. They are embedded
// in the binary, so no goose CLI is needed: `go run ./cmd/migrate up|down|status`.
package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/zabilal/gigmile-facility/internal/platform/config"
	"github.com/zabilal/gigmile-facility/migrations"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("migrate: %v", err)
	}
}

func run() error {
	command := "up"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return fmt.Errorf("connect to database (is `make up` running?): %w", err)
	}

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}

	if err := goose.Run(command, db, "."); err != nil {
		return fmt.Errorf("%s: %w", command, err)
	}
	return nil
}
