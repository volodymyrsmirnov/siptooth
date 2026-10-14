// Package doctor verifies the installation of the current user (spec section 11).
//
// Run is read-only: it never writes files, changes adapter state or applies SCO routing.
// Fix (doctor --fix) creates what is missing in ~/.siptooth/; it never overwrites an
// existing file and never runs sudo (joining group bluetooth is shown, not executed). Each
// check reports ok, warn, fail or skip with a one-line remedy. Network checks (DNS, TCP/TLS,
// STUN, OPTIONS) are observations: an unreachable network is a warning, never a failure.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/volodymyrsmirnov/siptooth/internal/bluez"
	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/host"
)

// Check is one line of the doctor report.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok, warn, fail, skip
	Detail string `json:"detail"`
	Remedy string `json:"remedy,omitempty"`
}

// networkTimeout bounds each network check.
const networkTimeout = 3 * time.Second

// Options selects what doctor inspects. Empty fields take the real system's values.
type Options struct {
	ConfigPath string
	UnitPath   string      // default ~/.config/systemd/user/siptooth.service
	LingerDir  string      // default /var/lib/systemd/linger
	Executable string      // default: this binary, symlinks resolved
	Identity   *Identity   // default: CurrentIdentity()
	Runner     host.Runner // default: host.ExecRunner discarding output
	NoNetwork  bool        // skip the DNS, TCP/TLS, NAT and OPTIONS checks
	// SkipPlatform omits the platform checks (bluetooth, D-Bus, /proc, linger, unit); tests set it
	// to stay off the real system.
	SkipPlatform bool
	Resolver     *net.Resolver
	// STUNServers overrides nat.stun_servers (tests point it at loopback servers).
	STUNServers []string
}

// Identity is the current user as far as the bluetooth group is concerned.
type Identity struct {
	Username     string
	UID          int
	Groups       []int // group ids of this process (real, effective and supplementary)
	BluetoothGID int   // -1 when the bluetooth group does not exist
	InGroupFile  bool  // member of bluetooth according to /etc/group (takes effect at next login)
}

// InBluetoothGroup reports whether this process already has the bluetooth group.
func (id *Identity) InBluetoothGroup() bool {
	return id.BluetoothGID >= 0 && slices.Contains(id.Groups, id.BluetoothGID)
}

// CurrentIdentity looks up the invoking user.
func CurrentIdentity() (*Identity, error) {
	u, err := user.Current()
	if err != nil {
		return nil, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	id := &Identity{Username: u.Username, UID: uid, BluetoothGID: -1}
	groups, _ := os.Getgroups()
	id.Groups = append(groups, os.Getgid(), os.Getegid())
	g, err := user.LookupGroup("bluetooth")
	if err != nil {
		return id, nil
	}
	id.BluetoothGID, _ = strconv.Atoi(g.Gid)
	if gids, err := u.GroupIds(); err == nil {
		id.InGroupFile = slices.Contains(gids, g.Gid)
	}
	return id, nil
}

func ok(name, detail string) Check { return Check{Name: name, Status: "ok", Detail: detail} }
func warn(name, detail, remedy string) Check {
	return Check{Name: name, Status: "warn", Detail: detail, Remedy: remedy}
}
func fail(name, detail, remedy string) Check {
	return Check{Name: name, Status: "fail", Detail: detail, Remedy: remedy}
}
func skip(name, detail string) Check { return Check{Name: name, Status: "skip", Detail: detail} }

// HasFailures reports whether any check failed (warnings do not count).
func HasFailures(checks []Check) bool {
	return slices.ContainsFunc(checks, func(c Check) bool { return c.Status == "fail" })
}

// buildCheck confirms the binary was built with CGO_ENABLED=0 (recorded by the Go toolchain).
func buildCheck() Check {
	info, found := debug.ReadBuildInfo()
	if !found {
		return warn("build", "no build info in binary", "build with `make build`")
	}
	for _, s := range info.Settings {
		if s.Key == "CGO_ENABLED" {
			if s.Value == "0" {
				return ok("build", "CGO_ENABLED=0 static binary")
			}
			return warn("build", "binary built with CGO_ENABLED="+s.Value, "rebuild with `make build`")
		}
	}
	return warn("build", "CGO_ENABLED not recorded (test binary?)", "rebuild with `make build`")
}

// dirUsable reports whether dir exists and is writable, or can be created by this user
// (its nearest existing ancestor is a writable directory). It never creates anything.
func dirUsable(dir string) (exists bool, err error) {
	fi, statErr := os.Stat(dir)
	if statErr == nil {
		if !fi.IsDir() {
			return true, fmt.Errorf("%s is not a directory", dir)
		}
		if err := unix.Access(dir, unix.W_OK|unix.X_OK); err != nil {
			return true, fmt.Errorf("%s is not writable by this user: %w", dir, err)
		}
		return true, nil
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		return false, statErr
	}
	parent := filepath.Dir(dir)
	if parent == dir {
		return false, statErr
	}
	if _, err := dirUsable(parent); err != nil {
		return false, fmt.Errorf("%s cannot be created: %w", dir, err)
	}
	return false, nil
}

// controlSocketCheck: the socket path fits sun_path and its directory is usable.
func controlSocketCheck(socket string) Check {
	if len(socket) > config.MaxSocketPathLen {
		return fail("control_socket", fmt.Sprintf("%s is too long (%d bytes, max %d)", socket, len(socket), config.MaxSocketPathLen), "set control.socket to a shorter absolute path")
	}
	dir := filepath.Dir(socket)
	exists, err := dirUsable(dir)
	switch {
	case err != nil:
		return fail("control_socket", err.Error(), "set control.socket to a path in a directory this user owns")
	case !exists:
		return ok("control_socket", socket+" (directory will be created)")
	}
	return ok("control_socket", socket)
}

// GroupRemedy is the one command an administrator runs once to give user BlueZ access.
// siptooth prints it and never runs it.
func GroupRemedy(username string) string {
	return "sudo usermod -aG bluetooth " + username + ", then log out and back in"
}

// lingerCheck: without lingering the user manager, and the service with it, only runs
// while the user is logged in. systemd marks lingering users with a file in dir.
func lingerCheck(dir, username string) Check {
	if _, err := os.Stat(filepath.Join(dir, username)); err == nil {
		return ok("linger", "enabled for "+username+": the service starts at boot")
	}
	return warn("linger", "not enabled for "+username+": the service runs only while "+username+" is logged in",
		"run `siptooth install` (it runs `loginctl enable-linger "+username+"`)")
}

// groupCheck: the daemon needs group bluetooth for BlueZ D-Bus access (root needs nothing).
func groupCheck(id *Identity) Check {
	switch {
	case id.UID == 0:
		return ok("bluetooth_group", "running as root")
	case id.BluetoothGID < 0:
		return fail("bluetooth_group", "group bluetooth does not exist", "install the bluez package (apt install bluez)")
	case id.InBluetoothGroup():
		return ok("bluetooth_group", id.Username+" is in group bluetooth")
	case id.InGroupFile:
		return warn("bluetooth_group", id.Username+" was added to group bluetooth but this session predates it",
			"a user service inherits groups from the user@"+strconv.Itoa(id.UID)+" manager when it starts: with linger enabled, reboot (or `sudo systemctl restart user@"+strconv.Itoa(id.UID)+"`); `newgrp bluetooth` only helps the current shell")
	}
	return fail("bluetooth_group", id.Username+" is not in group bluetooth", GroupRemedy(id.Username))
}

// readUnit returns the unit file followed by its drop-ins (<path>.d/*.conf in lexical
// order, as systemd applies them) and the drop-ins that set ExecStart, e.g. an override
// from `systemctl edit siptooth`. A missing unit file is returned as its read error.
func readUnit(path string) (content []byte, execDropIns []string, err error) {
	content, err = os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	files, _ := filepath.Glob(filepath.Join(path+".d", "*.conf")) // Glob returns sorted names
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if host.ParseUnit(string(b)).SetsExecStart {
			execDropIns = append(execDropIns, f)
		}
		content = append(append(content, '\n'), b...)
	}
	return content, execDropIns, nil
}

// serviceUnitCheck compares the installed user unit (with its drop-ins appended) with this
// binary and config. execDropIns are drop-ins overriding ExecStart: `siptooth install`
// rewrites only the unit file, so a stale override has to be edited by hand.
func serviceUnitCheck(path string, content []byte, readErr error, execDropIns []string, exe, configPath string) Check {
	if errors.Is(readErr, os.ErrNotExist) {
		return warn("service_unit", path+" not installed", "run `siptooth install`")
	}
	if readErr != nil {
		return warn("service_unit", "cannot read "+path+": "+readErr.Error(), "run `siptooth install`")
	}
	info := host.ParseUnit(string(content))
	var stale []string
	execStale := false
	if info.Executable != exe {
		stale = append(stale, fmt.Sprintf("ExecStart runs %s (this binary is %s)", info.Executable, exe))
		execStale = true
	}
	if info.Config != configPath {
		stale = append(stale, fmt.Sprintf("--config %s (checking %s)", info.Config, configPath))
		execStale = true
	}
	if len(stale) > 0 {
		remedy := "run `siptooth install` to refresh"
		if execStale && len(execDropIns) > 0 {
			remedy = "the ExecStart override in " + strings.Join(execDropIns, ", ") +
				" wins over the unit: fix or remove it (`systemctl --user edit siptooth`), then run `siptooth install`"
		}
		return warn("service_unit", path+" is stale: "+strings.Join(stale, "; "), remedy)
	}
	if len(execDropIns) > 0 {
		return ok("service_unit", path+" runs "+exe+" (ExecStart from "+strings.Join(execDropIns, ", ")+")")
	}
	return ok("service_unit", path+" runs "+exe)
}

// adapterRoutingCheck applies the routing plan for the adapter's manufacturer: Broadcom is
// read back, a known other vendor is skipped, and an unreadable manufacturer is a warning
// (never the non-Broadcom exemption).
func adapterRoutingCheck(manu uint16, manuErr error, read func() (uint8, error)) Check {
	switch bluez.PlanRouting(manu, manuErr) {
	case bluez.RoutingUnknown:
		return warn("sco_routing", "adapter manufacturer unknown ("+manuErr.Error()+"); SCO routing not verified and the daemon will not answer until it is",
			"check `bluetoothctl show` and `systemctl status bluetooth`, then rerun `siptooth doctor`")
	case bluez.RoutingNotNeeded:
		return skip("sco_routing", fmt.Sprintf("manufacturer %d is not Broadcom", manu))
	}
	return routingCheck(read)
}

// routingCheck is split out so it can be tested without hardware.
func routingCheck(read func() (uint8, error)) Check {
	v, err := read()
	if errors.Is(err, os.ErrPermission) {
		return warn("sco_routing", "cannot verify routing without CAP_NET_RAW (the service assumes HCI routing, as Raspberry Pi OS sets at boot)",
			"nothing to do unless calls are answered but silent: then check `systemctl status bthelper` and see docs/operations.md")
	}
	if err != nil {
		return fail("sco_routing", err.Error(), "check that the adapter is up: bluetoothctl show")
	}
	if v != 1 {
		return fail("sco_routing", fmt.Sprintf("SCO routing is %d, not HCI (1)", v), "start the service once (it applies the routing) or check the adapter firmware")
	}
	return ok("sco_routing", "SCO routed over HCI")
}

// parseBlueZVersion reads "bluetoothctl: 5.82" (or a bare "5.82") into major and minor.
func parseBlueZVersion(out string) (major, minor int, valid bool) {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return 0, 0, false
	}
	parts := strings.Split(fields[len(fields)-1], ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// experimentalEnabled reports whether main.conf sets Experimental=true in [General].
func experimentalEnabled(mainConf string) bool {
	section := ""
	enabled := false
	for _, line := range strings.Split(mainConf, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if found && section == "General" && strings.TrimSpace(key) == "Experimental" {
			enabled = strings.EqualFold(strings.TrimSpace(value), "true")
		}
	}
	return enabled
}

// blueZVersionCheck applies the version rules to `bluetoothctl --version` output and the
// contents of /etc/bluetooth/main.conf.
func blueZVersionCheck(versionOut, mainConf string) Check {
	major, minor, valid := parseBlueZVersion(versionOut)
	ver := strings.TrimSpace(versionOut)
	if !valid {
		return fail("bluetoothd", "cannot parse bluetoothctl --version output "+strconv.Quote(ver), "apt install bluez (>= 5.66)")
	}
	ver = fmt.Sprintf("%d.%d", major, minor)
	if major < 5 || (major == 5 && minor < 66) {
		return fail("bluetoothd", "BlueZ "+ver+" is too old", "upgrade to BlueZ >= 5.66")
	}
	if (major > 5 || minor >= 84) && experimentalEnabled(mainConf) {
		return warn("bluetoothd", "BlueZ "+ver+" with Experimental = true enables its own HFP plugin", "set Experimental = false in /etc/bluetooth/main.conf or run bluetoothd with -P hfp")
	}
	return ok("bluetoothd", "BlueZ "+ver)
}

// competingHandlerNames are processes that register their own HFP profile with BlueZ.
var competingHandlerNames = []string{"pipewire", "pulseaudio", "bluealsad", "bluealsa", "ofonod"}

// competingHandlersCheck scans procDir/*/comm (procDir is /proc in production).
func competingHandlersCheck(procDir string) Check {
	matches, _ := filepath.Glob(filepath.Join(procDir, "[0-9]*", "comm"))
	var found []string
	for _, p := range matches {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		n := strings.TrimSpace(string(b))
		if slices.Contains(competingHandlerNames, n) && !slices.Contains(found, n) {
			found = append(found, n)
		}
	}
	if len(found) > 0 {
		slices.Sort(found)
		return fail("competing_handlers", "running: "+strings.Join(found, ", "), "stop and disable them; they register the same HFP profile")
	}
	return ok("competing_handlers", "none running")
}

func (o *Options) fill() {
	if o.UnitPath == "" {
		home, _ := os.UserHomeDir()
		o.UnitPath = host.UserUnitPath(home, os.Getenv("XDG_CONFIG_HOME"))
	}
	if o.LingerDir == "" {
		o.LingerDir = "/var/lib/systemd/linger"
	}
	if o.Executable == "" {
		if p, err := os.Executable(); err == nil {
			if r, err := filepath.EvalSymlinks(p); err == nil {
				p = r
			}
			o.Executable = p
		}
	}
	if o.Identity == nil {
		if id, err := CurrentIdentity(); err == nil {
			o.Identity = id
		} else {
			o.Identity = &Identity{UID: os.Getuid(), BluetoothGID: -1}
		}
	}
	if o.Runner == nil {
		o.Runner = host.ExecRunner{Out: nil, Err: nil}
	}
	if abs, err := filepath.Abs(o.ConfigPath); err == nil {
		o.ConfigPath = abs
	}
}

// Run executes every check. Without a readable config only the config check is reported;
// a config that reads but does not validate (placeholders) fails and the rest still runs.
func Run(ctx context.Context, opts Options) []Check {
	opts.fill()
	var out []Check
	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		remedy := "fix " + opts.ConfigPath + " and run `siptooth config check`"
		if errors.Is(err, os.ErrNotExist) {
			remedy = "run `siptooth doctor --fix` to create it"
		}
		return append(out, fail("config", err.Error(), remedy))
	}
	password, perr := config.LookupPassword(opts.ConfigPath)
	if errs := config.Validate(cfg, password); len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		slices.Sort(msgs)
		out = append(out, fail("config", strings.Join(msgs, "; "), "edit "+opts.ConfigPath+" (`siptooth pair` fills in bluetooth.phone_address); `siptooth config check` lists every problem"))
	} else {
		out = append(out, ok("config", opts.ConfigPath))
	}
	envPath := config.EnvFilePath(opts.ConfigPath)
	out = append(out, envFileCheck(envPath))
	out = append(out, passwordCheck(cfg, password, perr, envPath))
	out = append(out, controlSocketCheck(cfg.Control.Socket))
	out = append(out, allochkaCheck(ctx, opts.Runner))
	if !opts.SkipPlatform {
		out = append(out, platformChecks(ctx, cfg, opts)...)
	}
	out = append(out, buildCheck())
	if !opts.NoNetwork {
		out = append(out, networkChecks(ctx, cfg, password, opts)...)
	}
	return out
}

// envFileCheck wants the env file to exist with mode 0600; anything wider warns.
func envFileCheck(path string) Check {
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return warn("env_file", path+" does not exist", "run `siptooth doctor --fix` to create it")
	}
	if err != nil {
		return fail("env_file", err.Error(), "check permissions on "+filepath.Dir(path))
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return warn("env_file", fmt.Sprintf("%s mode %04o is readable by others", path, perm), "chmod 0600 "+path+" (or `siptooth doctor --fix`)")
	}
	return ok("env_file", path+" mode 0600")
}

// passwordCheck: with sip.username set the digest password must be available, from the
// environment or the env file. Without a username none is needed.
func passwordCheck(cfg config.Config, password string, lookupErr error, envPath string) Check {
	switch {
	case lookupErr != nil:
		return fail("sip_password", lookupErr.Error(), "chmod 0600 "+envPath+" and make sure this user owns it")
	case cfg.SIP.Username == "":
		return skip("sip_password", "sip.username is not set: calls are placed without credentials")
	case password == "":
		return fail("sip_password", config.PasswordVar+" is not set (sip.username is "+cfg.SIP.Username+")",
			"put "+config.PasswordVar+"=... into "+envPath+" (mode 0600) or export it")
	}
	return ok("sip_password", config.PasswordVar+" is set")
}

// allochkaCheck: a running allochka user service also registers an HFP profile and would
// fight siptooth for the phone. `systemctl --user is-active --quiet allochka` exits 0 only
// when the unit is active; any failure (inactive, unknown unit, no systemd) means it is not.
func allochkaCheck(ctx context.Context, r host.Runner) Check {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := r.Run(ctx, host.Cmd{Argv: []string{"systemctl", "--user", "is-active", "--quiet", "allochka"}, Quiet: true})
	if err == nil {
		return fail("allochka_service", "the allochka service is running and holds the same HFP profile",
			"systemctl --user disable --now allochka")
	}
	return ok("allochka_service", "not running")
}
