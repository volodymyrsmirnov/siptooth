//go:build linux

package doctor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/bluez"
	"github.com/volodymyrsmirnov/siptooth/internal/config"
)

func platformChecks(ctx context.Context, cfg config.Config, opts Options) []Check {
	var out []Check
	out = append(out, groupCheck(opts.Identity))
	out = append(out, bluetoothdCheck(ctx))
	out = append(out, adapterChecks(ctx, cfg)...)
	out = append(out, competingHandlersCheck("/proc"))
	unit, execDropIns, err := readUnit(opts.UnitPath)
	out = append(out, serviceUnitCheck(opts.UnitPath, unit, err, execDropIns, opts.Executable, opts.ConfigPath))
	out = append(out, lingerCheck(opts.LingerDir, opts.Identity.Username))
	return out
}

func bluetoothdCheck(ctx context.Context) Check {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	outb, err := exec.CommandContext(ctx, "bluetoothctl", "--version").Output()
	if err != nil {
		return fail("bluetoothd", "bluetoothctl --version failed: "+err.Error(), "apt install bluez && systemctl enable --now bluetooth")
	}
	mainConf, _ := os.ReadFile("/etc/bluetooth/main.conf")
	return blueZVersionCheck(string(outb), string(mainConf))
}

// adapterChecks only reads adapter and device properties; it never powers or reconfigures anything.
func adapterChecks(ctx context.Context, cfg config.Config) []Check {
	m, err := bluez.Connect(ctx, cfg.Bluetooth.Adapter, cfg.Bluetooth.PhoneAddress, slog.Default())
	if err != nil {
		return []Check{
			fail("adapter", "bluetoothd not reachable on D-Bus or adapter missing: "+err.Error(), "check `systemctl status bluetooth` and `bluetoothctl show`; this user must be in group bluetooth (see bluetooth_group); if D-Bus still denies access, add a <policy group=\"bluetooth\"> allowing org.bluez in /etc/dbus-1/system.d/"),
			skip("adapter_powered", "no adapter"), skip("phone_bonded", "no adapter"), skip("sco_routing", "no adapter"),
		}
	}
	defer m.Close()
	manu, manuErr := m.Manufacturer()
	if manuErr != nil {
		manu, manuErr = m.RefreshManufacturer() // one retry: the adapter may just be appearing
	}
	manuName := fmt.Sprintf("manufacturer %d", manu)
	switch {
	case manuErr != nil:
		manuName = "manufacturer unknown"
	case manu == bluez.ManufacturerBroadcom:
		manuName += " (Broadcom)"
	}
	out := []Check{ok("adapter", fmt.Sprintf("%s %s %s", cfg.Bluetooth.Adapter, m.AdapterAddress(), manuName))}
	if on, err := m.Powered(); err != nil {
		out = append(out, fail("adapter_powered", "cannot read Powered: "+err.Error(), "bluetoothctl show"))
	} else if !on {
		out = append(out, fail("adapter_powered", "adapter is not powered", "bluetoothctl power on (the service powers it automatically)"))
	} else {
		out = append(out, ok("adapter_powered", "powered"))
	}
	paired, trusted, err := m.PhonePaired()
	switch {
	case cfg.Bluetooth.PhoneAddress == "":
		out = append(out, fail("phone_bonded", "bluetooth.phone_address is not set", "run `siptooth pair` (with the service stopped)"))
	case err != nil:
		out = append(out, fail("phone_bonded", cfg.Bluetooth.PhoneAddress+" unknown to BlueZ: "+err.Error(), "run `siptooth pair` with the service stopped"))
	case paired && trusted:
		out = append(out, ok("phone_bonded", cfg.Bluetooth.PhoneAddress+" paired and trusted"))
	case paired:
		out = append(out, warn("phone_bonded", cfg.Bluetooth.PhoneAddress+" paired but not trusted; auto-connect may prompt", "bluetoothctl trust "+cfg.Bluetooth.PhoneAddress))
	default:
		out = append(out, fail("phone_bonded", cfg.Bluetooth.PhoneAddress+" not paired", "siptooth pair"))
	}
	idx := m.AdapterIndex()
	out = append(out, adapterRoutingCheck(manu, manuErr, func() (uint8, error) { return bluez.ReadSCORouting(idx) }))
	return out
}
