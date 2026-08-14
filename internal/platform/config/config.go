// Package config loads service configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// ApplyMode selects how a payment reaches the ledger.
type ApplyMode string

const (
	// ApplySync commits the ledger entry and balance update inside the request.
	// Gives read-your-writes, which is what "instantly updating the current
	// position" asks for.
	ApplySync ApplyMode = "sync"
	// ApplyAsync durably records the payment in the request and lets a worker
	// pool apply it. Decouples our availability from the database's and allows
	// per-customer batching.
	ApplyAsync ApplyMode = "async"
)

const placeholderSecret = "dev-secret-do-not-use-in-production"

// Config is the fully resolved service configuration.
type Config struct {
	DatabaseURL  string
	HTTPAddr     string
	HMACSecret   string
	ApplyMode    ApplyMode
	LogLevel     string
	Environment  string
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	// MaxPoolConns bounds database connections held by this instance. Sized for
	// the machine, not the request rate: past a point more connections reduce
	// throughput rather than increase it.
	MaxPoolConns int32
}

// Load reads configuration from the environment, applying defaults that let a
// fresh clone run without a .env file.
func Load() (Config, error) {
	cfg := Config{
		DatabaseURL:  env("DATABASE_URL", "postgres://facility:facility@localhost:5433/facility?sslmode=disable"),
		HTTPAddr:     env("HTTP_ADDR", ":8080"),
		HMACSecret:   env("HMAC_SECRET", placeholderSecret),
		ApplyMode:    ApplyMode(strings.ToLower(env("APPLY_MODE", string(ApplySync)))),
		LogLevel:     env("LOG_LEVEL", "info"),
		Environment:  env("ENVIRONMENT", "development"),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		MaxPoolConns: 32,
	}

	switch cfg.ApplyMode {
	case ApplySync, ApplyAsync:
	default:
		return Config{}, fmt.Errorf("APPLY_MODE must be %q or %q, got %q", ApplySync, ApplyAsync, cfg.ApplyMode)
	}

	// Refuse to start with the shipped placeholder anywhere real. The webhook
	// lets a caller reduce a debt; a known signing key makes it a public one.
	if cfg.Environment != "development" && cfg.HMACSecret == placeholderSecret {
		return Config{}, fmt.Errorf("HMAC_SECRET is still the placeholder value in environment %q", cfg.Environment)
	}
	if cfg.HMACSecret == "" {
		return Config{}, fmt.Errorf("HMAC_SECRET must not be empty")
	}

	return cfg, nil
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
