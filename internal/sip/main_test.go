package sip

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestMain silences the process logger, which sipgo's default logger (it warns
// about connection reference counts while tests tear connections down)
// forwards to, so -v output stays clean.
func TestMain(m *testing.M) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	slog.SetDefault(quiet)
	os.Exit(m.Run())
}
