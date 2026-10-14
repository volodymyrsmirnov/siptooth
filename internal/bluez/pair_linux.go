//go:build linux

package bluez

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	agentMgrIface = "org.bluez.AgentManager1"
	agentIface    = "org.bluez.Agent1"
	pairTimeout   = 180 * time.Second
)

var errRejected = dbus.NewError("org.bluez.Error.Rejected", nil)

// agent implements org.bluez.Agent1 on the terminal (spec section 3.2 step 5).
type agent struct {
	mu  sync.Mutex // one prompt at a time: bluetoothd may call methods concurrently
	in  *bufio.Reader
	out io.Writer

	approvals approvals // pairing requests the user approved, and when
}

// note records an approved pairing request. It is called only on success, after the user
// answered: an already-bonded phone reports Paired=true while the prompt is still open, so a
// request that is merely in progress must not end the pairing run.
func (a *agent) note(device dbus.ObjectPath) {
	a.approvals.note(addrFromDevicePath(string(device)), time.Now())
}

func (a *agent) ask(prompt string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	fmt.Fprint(a.out, prompt)
	line, _ := a.in.ReadString('\n')
	return strings.TrimSpace(line)
}

func yes(ans string) bool { return strings.EqualFold(ans, "y") || strings.EqualFold(ans, "yes") }

func (a *agent) Release() *dbus.Error { return nil }
func (a *agent) Cancel() *dbus.Error  { fmt.Fprintln(a.out, "pairing cancelled by phone"); return nil }

func (a *agent) RequestPinCode(device dbus.ObjectPath) (string, *dbus.Error) {
	pin := a.ask(fmt.Sprintf("PIN code for %s: ", device))
	if pin == "" {
		return "", errRejected
	}
	a.note(device)
	return pin, nil
}

func (a *agent) DisplayPinCode(device dbus.ObjectPath, pin string) *dbus.Error {
	fmt.Fprintf(a.out, "Enter this PIN on the phone: %s\n", pin)
	return nil
}

func (a *agent) RequestPasskey(device dbus.ObjectPath) (uint32, *dbus.Error) {
	pk, err := strconv.ParseUint(a.ask(fmt.Sprintf("Passkey for %s: ", device)), 10, 32)
	if err != nil || pk > 999999 {
		return 0, errRejected
	}
	a.note(device)
	return uint32(pk), nil
}

func (a *agent) DisplayPasskey(device dbus.ObjectPath, passkey uint32, entered uint16) *dbus.Error {
	fmt.Fprintf(a.out, "Passkey: %06d\n", passkey)
	return nil
}

func (a *agent) RequestConfirmation(device dbus.ObjectPath, passkey uint32) *dbus.Error {
	if !yes(a.ask(fmt.Sprintf("Phone %s shows passkey %06d. Confirm? [y/N] ", device, passkey))) {
		return errRejected
	}
	a.note(device)
	return nil
}

func (a *agent) RequestAuthorization(device dbus.ObjectPath) *dbus.Error {
	if !yes(a.ask(fmt.Sprintf("Authorize pairing with %s? [y/N] ", device))) {
		return errRejected
	}
	a.note(device)
	return nil
}

// AuthorizeService accepts HFP (spec section 3.2 step 5) and rejects every other service.
func (a *agent) AuthorizeService(device dbus.ObjectPath, uuid string) *dbus.Error {
	switch strings.ToLower(uuid) {
	case HFPHFUUID, HFPAGUUID:
		fmt.Fprintf(a.out, "Authorizing HFP service %s for %s\n", uuid, device)
		a.note(device)
		return nil
	}
	fmt.Fprintf(a.out, "Rejecting service %s for %s\n", uuid, device)
	return errRejected
}

// pairingProfile is the HFP handler used while pairing. The profile is registered only so the
// phone's SDP query sees Hands-Free support (spec section 3.2 step 2); while no phone is
// configured profileObject.NewConnection accepts any device, so this handler refuses every
// connection: returning an error makes profileObject close the fd and fail the D-Bus call.
type pairingProfile struct{}

func (pairingProfile) NewConnection(device string, fd *os.File) error {
	return errors.New("pairing mode: HFP connections are not served")
}
func (pairingProfile) Disconnected(device string) {}

// pairedDevices lists addresses (upper case) of devices under the adapter with Paired=true.
func (m *Manager) pairedDevices(ctx context.Context) (map[string]bool, error) {
	var objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	if err := m.obj("/").CallWithContext(ctx, "org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&objs); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for path, ifaces := range objs {
		dev, ok := ifaces[deviceIface]
		if !ok || !strings.HasPrefix(string(path), string(m.adapterPath)+"/") {
			continue
		}
		paired, _ := dev["Paired"].Value().(bool)
		addr, _ := dev["Address"].Value().(string)
		if paired && addr != "" {
			out[strings.ToUpper(addr)] = true
		}
	}
	return out, nil
}

// DeviceState reports whether the device with the given address is Paired and Trusted.
func (m *Manager) DeviceState(ctx context.Context, addr string) (paired, trusted bool, err error) {
	dev := dbus.ObjectPath(DevicePath(m.adapterName, addr))
	if err = m.getProp(ctx, dev, deviceIface, "Paired", &paired); err != nil {
		return false, false, err
	}
	err = m.getProp(ctx, dev, deviceIface, "Trusted", &trusted)
	return paired, trusted, err
}

// Pair runs the interactive pairing flow (spec section 3.2) and returns the new phone's address.
func (m *Manager) Pair(ctx context.Context, in io.Reader, out io.Writer) (string, error) {
	ag := &agent{in: bufio.NewReader(in), out: out}
	if err := m.conn.Export(ag, agentPath, agentIface); err != nil {
		return "", fmt.Errorf("export agent: %w", err)
	}
	defer m.conn.Export(nil, agentPath, agentIface)
	mgr := m.obj("/org/bluez")
	if c := mgr.CallWithContext(ctx, agentMgrIface+".RegisterAgent", 0, dbus.ObjectPath(agentPath), "KeyboardDisplay"); c.Err != nil {
		return "", fmt.Errorf("RegisterAgent: %w", c.Err)
	}
	defer mgr.Call(agentMgrIface+".UnregisterAgent", 0, dbus.ObjectPath(agentPath))
	if c := mgr.CallWithContext(ctx, agentMgrIface+".RequestDefaultAgent", 0, dbus.ObjectPath(agentPath)); c.Err != nil {
		return "", fmt.Errorf("RequestDefaultAgent: %w", c.Err)
	}
	if err := m.RegisterProfile(ctx, pairingProfile{}); err != nil {
		return "", err
	}
	defer m.conn.Export(nil, profilePath, profileIface)
	defer mgr.Call(profileMgrIface+".UnregisterProfile", 0, dbus.ObjectPath(profilePath))
	before, err := m.pairedDevices(ctx)
	if err != nil {
		return "", fmt.Errorf("list devices: %w", err)
	}
	if err := m.SetPowered(true); err != nil {
		return "", fmt.Errorf("power on: %w", err)
	}
	if err := m.SetAlias(pairAlias); err != nil {
		return "", fmt.Errorf("set alias: %w", err)
	}
	if err := m.SetPairable(true); err != nil {
		return "", fmt.Errorf("set pairable: %w", err)
	}
	if err := m.SetDiscoverable(true, uint32(pairTimeout/time.Second)); err != nil {
		return "", fmt.Errorf("set discoverable: %w", err)
	}
	defer m.SetDiscoverable(false, 0)
	fmt.Fprintf(out, "On your phone, open Bluetooth settings and pair with %q.\n", pairAlias)
	fmt.Fprintf(out, "Waiting up to %d seconds...\n", int(pairTimeout/time.Second))
	deadline := time.NewTimer(pairTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			return "", fmt.Errorf("no new device paired within %d seconds", int(pairTimeout/time.Second))
		case <-tick.C:
		}
		now, err := m.pairedDevices(ctx)
		if err != nil {
			continue
		}
		if addr, ok := selectPairedDevice(before, now, ag.approvals.settled(time.Now(), bondSettle)); ok {
			// Trust via the device path directly rather than assigning m.phonePath, which
			// profileObject.NewConnection reads concurrently on the D-Bus goroutine.
			dev := dbus.ObjectPath(DevicePath(m.adapterName, addr))
			if err := m.setProp(dev, deviceIface, "Trusted", true); err != nil {
				return addr, fmt.Errorf("trust %s: %w", addr, err)
			}
			if err := m.SetDiscoverable(false, 0); err != nil {
				fmt.Fprintf(out, "warning: could not turn discoverable off: %v\n", err)
			}
			fmt.Fprintf(out, "Paired and trusted %s\n", addr)
			return addr, nil
		}
	}
}
