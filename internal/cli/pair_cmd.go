package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/bluez"
	"github.com/volodymyrsmirnov/siptooth/internal/config"
)

func init() {
	register(command{"pair", "Pair the phone interactively and record its address (daemon must be stopped)", runPair})
}

// daemonRunning reports whether something accepts connections on the control socket.
func daemonRunning(path string) bool {
	c, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func runPair(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfigPath(), "configuration file (its phone_address is updated)")
	adapterFlag := fs.String("adapter", "", "Bluetooth adapter (default: bluetooth.adapter from the config)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	// The config is read without validating it: phone_address is still empty before pairing.
	cfg, haveCfg := config.Default(), false
	if loaded, err := config.Load(*cfgPath); err == nil {
		cfg, haveCfg = loaded, true
	}
	adapter := cfg.Bluetooth.Adapter
	if *adapterFlag != "" {
		adapter = *adapterFlag
	}
	if haveCfg && daemonRunning(cfg.Control.Socket) {
		fmt.Fprintln(stderr, "stop the siptooth service first (systemctl --user stop siptooth)")
		return ExitError
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	m, err := bluez.Connect(ctx, adapter, "", slog.Default())
	if err != nil {
		fmt.Fprintln(stderr, "bluetooth:", err)
		return ExitError
	}
	defer m.Close()
	if phone := cfg.Bluetooth.PhoneAddress; phone != "" {
		if paired, trusted, serr := m.DeviceState(ctx, phone); serr == nil && paired && trusted {
			fmt.Fprintf(stdout, "phone %s already paired; pairing again is only needed if the phone forgot this device\n", phone)
		}
	}
	addr, err := m.Pair(ctx, os.Stdin, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "pairing failed:", err)
		return ExitError
	}
	if !haveCfg {
		fmt.Fprintf(stdout, "\nAdd to %s:\nbluetooth.phone_address: %q\n", *cfgPath, addr)
		return ExitOK
	}
	if err := setPhoneAddress(*cfgPath, addr); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitError
	}
	fmt.Fprintf(stdout, "\nUpdated %s: bluetooth.phone_address %q\n", *cfgPath, addr)
	return ExitOK
}
