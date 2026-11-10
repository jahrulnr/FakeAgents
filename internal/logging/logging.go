// Package logging builds the structured logger used by the CLI and server.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/jahrulnr/fakeagents/internal/config"
)

// New creates a logger and an optional closer for file outputs.
func New(cfg config.Logging) (*slog.Logger, io.Closer, error) {
	var writer io.Writer
	var closer io.Closer
	switch strings.ToLower(strings.TrimSpace(cfg.Output)) {
	case "", "stdout":
		writer = os.Stdout
	case "stderr":
		writer = os.Stderr
	case "discard":
		writer = io.Discard
	default:
		file, err := os.OpenFile(cfg.Output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, nil, fmt.Errorf("open log file: %w", err)
		}
		writer = file
		closer = file
	}

	options := &slog.HandlerOptions{Level: parseLevel(cfg.Level)}
	var handler slog.Handler
	if strings.EqualFold(cfg.Format, "json") {
		handler = slog.NewJSONHandler(writer, options)
	} else {
		handler = slog.NewTextHandler(writer, options)
	}
	return slog.New(handler), closer, nil
}

func parseLevel(value string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
