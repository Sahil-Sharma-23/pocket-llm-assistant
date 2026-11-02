// Package logging builds the application's structured slog.Logger, writing
// either to stderr or to a size-rotated file.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"personal-assistant/internal/config"
)

// New returns a logger configured from cfg and a close function that must be
// called on shutdown to flush and release the log file.
func New(cfg config.LogConfig, stderr io.Writer) (*slog.Logger, func() error, error) {
	level, err := ParseLevel(cfg.Level)
	if err != nil {
		return nil, nil, err
	}

	out := stderr
	closeFn := func() error { return nil }
	if cfg.File != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.File), 0o700); err != nil {
			return nil, nil, fmt.Errorf("create log dir: %w", err)
		}
		rf, err := NewRotatingFile(cfg.File, int64(cfg.MaxSizeMB)*1024*1024, cfg.MaxBackups)
		if err != nil {
			return nil, nil, err
		}
		out = rf
		closeFn = rf.Close
	}

	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	switch cfg.Format {
	case "text":
		h = slog.NewTextHandler(out, opts)
	default:
		h = slog.NewJSONHandler(out, opts)
	}
	return slog.New(h), closeFn, nil
}

// ParseLevel converts a config level string into an slog.Level.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q", s)
	}
}
