//go:build !linux

package hooks

func hasAmbientCaps() bool { return false }
