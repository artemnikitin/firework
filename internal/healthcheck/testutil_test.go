package healthcheck

import (
	"io"
	"log/slog"
)

// noopLogger returns a logger that discards all output, for use in tests.
func noopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
