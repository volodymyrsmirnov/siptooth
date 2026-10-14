//go:build !linux

package sco

import (
	"errors"
	"testing"
)

// Off Linux there is no Bluetooth: Listen reports ErrUnsupported (never touches an adapter).
// On Linux this test would open a real SCO listener, so it does not build there.
func TestListenerUnsupportedOffLinux(t *testing.T) {
	if _, err := Listen("AA:BB:CC:DD:EE:FF"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Listen = %v, want ErrUnsupported", err)
	}
}
