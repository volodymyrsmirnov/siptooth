//go:build linux

package bluez

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

const (
	bluezService    = "org.bluez"
	adapterIface    = "org.bluez.Adapter1"
	deviceIface     = "org.bluez.Device1"
	profileMgrIface = "org.bluez.ProfileManager1"
	profileIface    = "org.bluez.Profile1"
	propsIface      = "org.freedesktop.DBus.Properties"
)

// Manager wraps one adapter and one configured phone.
type Manager struct {
	conn        *dbus.Conn
	log         *slog.Logger
	adapterName string
	adapterPath dbus.ObjectPath
	adapterIdx  int
	phonePath   dbus.ObjectPath

	mu           sync.Mutex
	address      string
	manufacturer uint16
	manuErr      error // non-nil while Adapter1.Manufacturer could not be read
}

// Connect opens the system bus and reads adapter properties. phone may be empty for pairing.
func Connect(ctx context.Context, adapter, phone string, logger *slog.Logger) (*Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}
	idx, err := AdapterIndexFromName(adapter)
	if err != nil {
		return nil, err
	}
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("system bus: %w", err)
	}
	m := &Manager{conn: conn, log: logger, adapterName: adapter, adapterPath: dbus.ObjectPath("/org/bluez/" + adapter), adapterIdx: idx}
	if phone != "" {
		m.phonePath = dbus.ObjectPath(DevicePath(adapter, phone))
	}
	if err := m.refreshAdapter(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return m, nil
}

func (m *Manager) obj(path dbus.ObjectPath) dbus.BusObject { return m.conn.Object(bluezService, path) }

// getProp reads one property with org.freedesktop.DBus.Properties.Get, bounded by ctx.
func (m *Manager) getProp(ctx context.Context, path dbus.ObjectPath, iface, name string, out any) error {
	var v dbus.Variant
	if err := m.obj(path).CallWithContext(ctx, propsIface+".Get", 0, iface, name).Store(&v); err != nil {
		return fmt.Errorf("get %s.%s on %s: %w", iface, name, path, err)
	}
	return v.Store(out)
}

// propReadTimeout bounds a property read that has no caller deadline.
const propReadTimeout = 5 * time.Second

func (m *Manager) setProp(path dbus.ObjectPath, iface, name string, value any) error {
	return m.obj(path).Call(propsIface+".Set", 0, iface, name, dbus.MakeVariant(value)).Err
}

func (m *Manager) refreshAdapter(ctx context.Context) error {
	var addr string
	if err := m.getProp(ctx, m.adapterPath, adapterIface, "Address", &addr); err != nil {
		return fmt.Errorf("adapter %s not found: %w", m.adapterName, err)
	}
	var manu uint16
	manuErr := m.getProp(ctx, m.adapterPath, adapterIface, "Manufacturer", &manu)
	if manuErr != nil {
		m.log.Warn("bluez: adapter manufacturer unreadable; SCO routing readiness stays unverified", "err", manuErr)
	}
	m.mu.Lock()
	m.address, m.manufacturer, m.manuErr = addr, manu, manuErr
	m.mu.Unlock()
	return nil
}

// RefreshManufacturer re-reads Adapter1.Manufacturer (discovery retry) and returns the result.
func (m *Manager) RefreshManufacturer() (uint16, error) {
	var manu uint16
	ctx, cancel := context.WithTimeout(context.Background(), propReadTimeout)
	defer cancel()
	err := m.getProp(ctx, m.adapterPath, adapterIface, "Manufacturer", &manu)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		m.manufacturer = manu
	}
	m.manuErr = err
	return m.manufacturer, m.manuErr
}

func (m *Manager) AdapterAddress() string { m.mu.Lock(); defer m.mu.Unlock(); return m.address }
func (m *Manager) AdapterIndex() int      { return m.adapterIdx }

// Manufacturer returns the cached Adapter1.Manufacturer and the error of the last read;
// the value is meaningless while the error is non-nil (see PlanRouting).
func (m *Manager) Manufacturer() (uint16, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.manufacturer, m.manuErr
}
func (m *Manager) Close() error { return m.conn.Close() }

// SetPowered, SetAlias, SetDiscoverable, SetPairable configure the adapter.
func (m *Manager) SetPowered(on bool) error {
	return m.setProp(m.adapterPath, adapterIface, "Powered", on)
}
func (m *Manager) SetAlias(alias string) error {
	return m.setProp(m.adapterPath, adapterIface, "Alias", alias)
}
func (m *Manager) SetDiscoverable(on bool, timeoutSec uint32) error {
	if err := m.setProp(m.adapterPath, adapterIface, "DiscoverableTimeout", timeoutSec); err != nil {
		return err
	}
	return m.setProp(m.adapterPath, adapterIface, "Discoverable", on)
}
func (m *Manager) SetPairable(on bool) error {
	return m.setProp(m.adapterPath, adapterIface, "Pairable", on)
}

func (m *Manager) PhoneConnected() bool {
	var v bool
	if err := m.getProp(context.Background(), m.phonePath, deviceIface, "Connected", &v); err != nil {
		return false
	}
	return v
}

func (m *Manager) PhonePaired() (paired, trusted bool, err error) {
	if err = m.getProp(context.Background(), m.phonePath, deviceIface, "Paired", &paired); err != nil {
		return
	}
	err = m.getProp(context.Background(), m.phonePath, deviceIface, "Trusted", &trusted)
	return
}

func (m *Manager) SetPhoneTrusted(on bool) error {
	return m.setProp(m.phonePath, deviceIface, "Trusted", on)
}

// ConnectProfile asks BlueZ to connect to the phone's Audio Gateway service; BlueZ pairs the
// remote AG UUID with our registered HF profile (src/profile.c maps local HS/HF to remote AG).
func (m *Manager) ConnectProfile(ctx context.Context) error {
	return m.obj(m.phonePath).CallWithContext(ctx, deviceIface+".ConnectProfile", 0, HFPAGUUID).Err
}

func (m *Manager) DisconnectProfile(ctx context.Context) error {
	return m.obj(m.phonePath).CallWithContext(ctx, deviceIface+".DisconnectProfile", 0, HFPAGUUID).Err
}

func (m *Manager) Powered() (bool, error) {
	var v bool
	err := m.getProp(context.Background(), m.adapterPath, adapterIface, "Powered", &v)
	return v, err
}

// --- Profile1 ---

type profileObject struct {
	m *Manager
	h ProfileHandler
}

// Release is bluetoothd telling us the profile is gone (it is shutting down or we were unregistered):
// tear down the live connection like a disconnect so the coordinator sees Disconnected.
func (p *profileObject) Release() *dbus.Error {
	p.m.log.Info("bluez: profile released")
	p.h.Disconnected(string(p.m.phonePath))
	return nil
}

func (p *profileObject) NewConnection(device dbus.ObjectPath, fd dbus.UnixFD, props map[string]dbus.Variant) *dbus.Error {
	if p.m.phonePath != "" && device != p.m.phonePath {
		unix.Close(int(fd))
		p.m.log.Warn("bluez: rejected HFP connection from unexpected device", "device", device)
		return dbus.MakeFailedError(fmt.Errorf("unexpected device %s", device))
	}
	if err := unix.SetNonblock(int(fd), true); err != nil {
		unix.Close(int(fd))
		return dbus.MakeFailedError(err)
	}
	f := os.NewFile(uintptr(fd), "rfcomm")
	if err := p.h.NewConnection(string(device), f); err != nil {
		f.Close()
		return dbus.MakeFailedError(err)
	}
	return nil
}

func (p *profileObject) RequestDisconnection(device dbus.ObjectPath) *dbus.Error {
	p.h.Disconnected(string(device))
	return nil
}

// RegisterProfile exports Profile1 and registers the HFP HF record with bluetoothd.
func (m *Manager) RegisterProfile(ctx context.Context, h ProfileHandler) error {
	p := &profileObject{m: m, h: h}
	if err := m.conn.Export(p, profilePath, profileIface); err != nil {
		return fmt.Errorf("export profile: %w", err)
	}
	opts := map[string]dbus.Variant{
		"Name":                  dbus.MakeVariant(profileName),
		"Version":               dbus.MakeVariant(uint16(0x0107)),
		"Features":              dbus.MakeVariant(uint16(0x0004)),
		"RequireAuthentication": dbus.MakeVariant(true),
		"RequireAuthorization":  dbus.MakeVariant(false),
	}
	call := m.obj("/org/bluez").CallWithContext(ctx, profileMgrIface+".RegisterProfile", 0, dbus.ObjectPath(profilePath), HFPHFUUID, opts)
	if call.Err != nil && !AlreadyRegistered(call.Err) {
		return fmt.Errorf("RegisterProfile: %w", call.Err)
	}
	m.log.Info("bluez: HFP HF profile registered", "path", profilePath)
	return nil
}

// Watch delivers adapter/phone/bluetoothd changes until ctx ends.
func (m *Manager) Watch(ctx context.Context, fn func(Change)) {
	sigs := make(chan *dbus.Signal, 32)
	m.conn.Signal(sigs)
	rules := []struct {
		name string
		opts []dbus.MatchOption
	}{
		{"PropertiesChanged", []dbus.MatchOption{dbus.WithMatchSender(bluezService), dbus.WithMatchInterface(propsIface), dbus.WithMatchMember("PropertiesChanged")}},
		{"InterfacesAdded", []dbus.MatchOption{dbus.WithMatchSender(bluezService), dbus.WithMatchInterface("org.freedesktop.DBus.ObjectManager"), dbus.WithMatchMember("InterfacesAdded")}},
		{"NameOwnerChanged", []dbus.MatchOption{dbus.WithMatchInterface("org.freedesktop.DBus"), dbus.WithMatchMember("NameOwnerChanged"), dbus.WithMatchArg(0, bluezService)}},
	}
	for _, r := range rules {
		// A missing rule silently disables adapter/bluetoothd change detection, so make it loud.
		if err := m.conn.AddMatchSignal(r.opts...); err != nil {
			m.log.Error("bluez: add D-Bus match rule failed", "rule", r.name, "err", err)
		}
	}
	defer m.conn.RemoveSignal(sigs)
	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-sigs:
			if !ok {
				return
			}
			m.handleSignal(s, fn)
		}
	}
}

func (m *Manager) handleSignal(s *dbus.Signal, fn func(Change)) {
	switch s.Name {
	case propsIface + ".PropertiesChanged":
		if len(s.Body) < 2 {
			return
		}
		iface, _ := s.Body[0].(string)
		changed, _ := s.Body[1].(map[string]dbus.Variant)
		if iface == adapterIface && s.Path == m.adapterPath {
			if v, ok := changed["Powered"]; ok {
				if on, _ := v.Value().(bool); on {
					_ = m.refreshAdapter(context.Background())
					fn(Change{Kind: AdapterUp})
				} else {
					fn(Change{Kind: AdapterDown})
				}
			}
		}
		if iface == deviceIface && m.phonePath != "" && s.Path == m.phonePath {
			if v, ok := changed["Connected"]; ok {
				if on, _ := v.Value().(bool); on {
					fn(Change{Kind: PhoneConnected})
				} else {
					fn(Change{Kind: PhoneDisconnected})
				}
			}
		}
	case "org.freedesktop.DBus.ObjectManager.InterfacesAdded":
		if len(s.Body) >= 1 {
			if p, ok := s.Body[0].(dbus.ObjectPath); ok && p == m.adapterPath {
				_ = m.refreshAdapter(context.Background())
				fn(Change{Kind: AdapterUp})
			}
		}
	case "org.freedesktop.DBus.NameOwnerChanged":
		if len(s.Body) == 3 {
			// The connection may receive NameOwnerChanged for other names (other match rules
			// on the shared connection); only bluetoothd's well-known name matters here.
			if name, _ := s.Body[0].(string); name != bluezService {
				return
			}
			if newOwner, _ := s.Body[2].(string); newOwner != "" {
				fn(Change{Kind: BluezRestarted})
			} else {
				fn(Change{Kind: BluezLost})
			}
		}
	}
}
