package cli

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// logLevelVar selects the log level (spec section 12).
const logLevelVar = "SIPTOOTH_LOG_LEVEL"

var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// logLevelFromEnv reads SIPTOOTH_LOG_LEVEL; empty means info, an unknown value is an
// error so a typo does not silently hide debug output.
func logLevelFromEnv() (slog.Level, bool) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(logLevelVar)))
	if v == "" {
		return slog.LevelInfo, true
	}
	l, ok := logLevels[v]
	return l, ok
}

// newLogger returns a text logger on w (stderr, which journald collects).
func newLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}
