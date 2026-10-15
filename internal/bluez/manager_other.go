//go:build !linux

package bluez

import (
	"context"
	"io"
	"log/slog"
)

type Manager struct{}

func Connect(ctx context.Context, adapter, phone string, logger *slog.Logger) (*Manager, error) {
	return nil, ErrUnsupported
}
func (m *Manager) RegisterProfile(ctx context.Context, h ProfileHandler) error { return ErrUnsupported }
func (m *Manager) RegisterMNSProfile(ctx context.Context, h ProfileHandler) error {
	return ErrUnsupported
}
func (m *Manager) UnregisterMNSProfile(ctx context.Context) error { return ErrUnsupported }
func (m *Manager) ConnectProfile(ctx context.Context) error       { return ErrUnsupported }
func (m *Manager) DisconnectProfile(ctx context.Context) error    { return ErrUnsupported }
func (m *Manager) AdapterAddress() string                         { return "" }
func (m *Manager) AdapterIndex() int                              { return 0 }
func (m *Manager) Manufacturer() (uint16, error)                  { return 0, ErrUnsupported }
func (m *Manager) RefreshManufacturer() (uint16, error)           { return 0, ErrUnsupported }
func (m *Manager) PhoneConnected() bool                           { return false }
func (m *Manager) PhonePaired() (bool, bool, error)               { return false, false, ErrUnsupported }
func (m *Manager) Powered() (bool, error)                         { return false, ErrUnsupported }
func (m *Manager) SetPhoneTrusted(bool) error                     { return ErrUnsupported }
func (m *Manager) SetPowered(bool) error                          { return ErrUnsupported }
func (m *Manager) SetAlias(string) error                          { return ErrUnsupported }
func (m *Manager) SetDiscoverable(bool, uint32) error             { return ErrUnsupported }
func (m *Manager) SetPairable(bool) error                         { return ErrUnsupported }
func (m *Manager) Watch(ctx context.Context, fn func(Change))     {}
func (m *Manager) Pair(ctx context.Context, in io.Reader, out io.Writer) (string, error) {
	return "", ErrUnsupported
}
func (m *Manager) DeviceState(ctx context.Context, addr string) (bool, bool, error) {
	return false, false, ErrUnsupported
}
func (m *Manager) Close() error { return nil }
func EnsureSCORouting(hciIndex int, logger *slog.Logger) (uint8, uint8, error) {
	return 0, 0, ErrUnsupported
}
func ReadSCORouting(hciIndex int) (uint8, error) { return 0, ErrUnsupported }
