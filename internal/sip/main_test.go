package sip

import (
	"io"
	"log/slog"
	"os"
	"testing"

	gosip "github.com/emiago/sipgo/sip"
)

// TestMain silences sipgo's default logger (it warns about connection
// reference counts while tests tear connections down) so -v output stays clean.
func TestMain(m *testing.M) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	gosip.SetDefaultLogger(quiet)
	slog.SetDefault(quiet)
	os.Exit(m.Run())
}
