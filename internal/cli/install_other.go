//go:build !linux

package cli

// installSupported: `siptooth install` drives the systemd user manager, which only exists on Linux.
var installSupported = false
