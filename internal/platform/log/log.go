// Package log builds the service logger.
package log

import (
	"log/slog"
	"os"
	"strings"
)

// New returns a JSON structured logger. JSON because an incident is
// investigated by querying for a transaction_reference, not by scrolling.
func New(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
