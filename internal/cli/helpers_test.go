package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// shortTempDir is short enough for a Unix socket path on macOS.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "stc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// validConfigBody validates without a password.
const validConfigBody = "bluetooth: {phone_address: 'AA:BB:CC:DD:EE:FF'}\nsip: {target: 'sip:600@sip.example.com;transport=tls'}\n"

// writeTempConfig writes body (validConfigBody when empty) to a new config.yaml.
func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	if body == "" {
		body = validConfigBody
	}
	p := filepath.Join(shortTempDir(t), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
