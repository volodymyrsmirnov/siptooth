package doctor

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"io/fs"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/host"
	"github.com/volodymyrsmirnov/siptooth/internal/sip/siptest"
	"github.com/volodymyrsmirnov/siptooth/internal/sipaddr"
)

// shortDir is a temporary directory short enough for a Unix socket path on macOS.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func find(checks []Check, name string) Check {
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	return Check{Name: name, Status: "missing"}
}

var member = &Identity{Username: "pi", UID: 1000, Groups: []int{1000, 112}, BluetoothGID: 112, InGroupFile: true}

func TestEnvFileCheck(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "env")
	if c := envFileCheck(p); c.Status != "warn" || !strings.Contains(c.Remedy, "doctor --fix") {
		t.Fatalf("missing: %+v", c)
	}
	os.WriteFile(p, []byte("SIPTOOTH_SIP_PASSWORD=x\n"), 0o600)
	os.Chmod(p, 0o600)
	if c := envFileCheck(p); c.Status != "ok" {
		t.Fatalf("0600: %+v", c)
	}
	for _, mode := range []os.FileMode{0o640, 0o644, 0o604} {
		os.Chmod(p, mode)
		if c := envFileCheck(p); c.Status != "warn" || !strings.Contains(c.Remedy, "chmod 0600 "+p) {
			t.Fatalf("%04o: %+v", mode, c)
		}
	}
}
func TestControlSocketCheck(t *testing.T) {
	dir := shortDir(t)
	if c := controlSocketCheck(filepath.Join(dir, "siptooth.sock")); c.Status != "ok" {
		t.Fatalf("%+v", c)
	}
	if c := controlSocketCheck(filepath.Join(dir, "new", "siptooth.sock")); c.Status != "ok" {
		t.Fatalf("creatable dir: %+v", c)
	}
	if c := controlSocketCheck("/" + strings.Repeat("x", 120) + ".sock"); c.Status != "fail" || !strings.Contains(c.Detail, "too long") {
		t.Fatalf("long: %+v", c)
	}
}

func TestGroupCheck(t *testing.T) {
	cases := []struct {
		name   string
		id     Identity
		status string
		remedy string
	}{
		{"member", Identity{Username: "pi", UID: 1000, Groups: []int{1000, 112}, BluetoothGID: 112, InGroupFile: true}, "ok", ""},
		{"root", Identity{Username: "root", UID: 0, Groups: []int{0}, BluetoothGID: 112}, "ok", ""},
		{"needs relogin", Identity{Username: "pi", UID: 1000, Groups: []int{1000}, BluetoothGID: 112, InGroupFile: true}, "warn", "newgrp bluetooth"},
		{"not member", Identity{Username: "pi", UID: 1000, Groups: []int{1000}, BluetoothGID: 112}, "fail", "sudo usermod -aG bluetooth pi"},
		{"no group", Identity{Username: "pi", UID: 1000, Groups: []int{1000}, BluetoothGID: -1}, "fail", "bluez"},
	}
	for _, c := range cases {
		got := groupCheck(&c.id)
		if got.Status != c.status || !strings.Contains(got.Remedy, c.remedy) {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
	if r := groupCheck(&cases[2].id).Remedy; !strings.Contains(r, "reboot") || !strings.Contains(r, "user@1000") || strings.Contains(r, "unit") {
		t.Errorf("re-login case must say reboot or restart user@UID: %q", r)
	}
}

func TestServiceUnitCheck(t *testing.T) {
	exe, cfg := "/home/pi/.local/bin/siptooth", "/home/pi/.siptooth/config.yaml"
	unit := host.Unit(host.UnitParams{ConfigPath: cfg, Executable: exe})
	if c := serviceUnitCheck("/u", nil, fs.ErrNotExist, nil, exe, cfg); c.Status != "warn" || c.Remedy != "run `siptooth install`" {
		t.Fatalf("missing: %+v", c)
	}
	if c := serviceUnitCheck("/u", []byte(unit), nil, nil, exe, cfg); c.Status != "ok" {
		t.Fatalf("current: %+v", c)
	}
	stale := []struct{ exe, cfg string }{
		{"/home/pi/bin/siptooth", cfg}, // binary moved
		{exe, "/home/pi/other/config.yaml"},
	}
	for _, s := range stale {
		if c := serviceUnitCheck("/u", []byte(unit), nil, nil, s.exe, s.cfg); c.Status != "warn" || c.Remedy != "run `siptooth install` to refresh" {
			t.Errorf("%+v: %+v", s, c)
		}
	}
}

func TestLingerCheck(t *testing.T) {
	dir := shortDir(t)
	c := lingerCheck(dir, "pi")
	if c.Status != "warn" || !strings.Contains(c.Detail, "logged in") || !strings.Contains(c.Remedy, "siptooth install") || !strings.Contains(c.Remedy, "loginctl enable-linger pi") || strings.Contains(c.Remedy, "sudo") {
		t.Fatalf("off: %+v", c)
	}
	os.WriteFile(filepath.Join(dir, "pi"), nil, 0o644)
	if c := lingerCheck(dir, "pi"); c.Status != "ok" {
		t.Fatalf("on: %+v", c)
	}
}

func TestReadUnitWithDropIns(t *testing.T) {
	dir := shortDir(t)
	unitPath := filepath.Join(dir, "siptooth.service")
	if _, _, err := readUnit(unitPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing unit: %v", err)
	}
	exe, cfg := "/usr/local/bin/siptooth", "/home/pi/.siptooth/config.yaml"
	os.WriteFile(unitPath, []byte(host.Unit(host.UnitParams{ConfigPath: cfg, Executable: exe})), 0o644)
	content, dropIns, err := readUnit(unitPath)
	if err != nil || len(dropIns) != 0 {
		t.Fatalf("%v %v", dropIns, err)
	}
	if c := serviceUnitCheck(unitPath, content, err, dropIns, exe, cfg); c.Status != "ok" {
		t.Fatalf("no drop-ins: %+v", c)
	}
	// A `systemctl edit` override keeps its own ExecStart path; files apply in lexical order.
	os.MkdirAll(unitPath+".d", 0o755)
	os.WriteFile(filepath.Join(unitPath+".d", "10-env.conf"), []byte("[Service]\nEnvironment=X=1\n"), 0o644)
	override := filepath.Join(unitPath+".d", "override.conf")
	os.WriteFile(override, []byte("[Service]\nExecStart=\nExecStart=/home/pi/bin/siptooth run --config "+cfg+" --log-level debug\n"), 0o644)
	os.WriteFile(filepath.Join(unitPath+".d", "ignored.txt"), []byte("ExecStart=/nope\n"), 0o644)
	content, dropIns, err = readUnit(unitPath)
	if err != nil || len(dropIns) != 1 || dropIns[0] != override {
		t.Fatalf("drop-ins %v %v", dropIns, err)
	}
	c := serviceUnitCheck(unitPath, content, err, dropIns, exe, cfg)
	if c.Status != "warn" || !strings.Contains(c.Detail, "/home/pi/bin/siptooth") || !strings.Contains(c.Remedy, override) || !strings.Contains(c.Remedy, "systemctl --user edit siptooth") {
		t.Fatalf("stale override: %+v", c)
	}
	// An override that runs this binary is fine.
	os.WriteFile(override, []byte("[Service]\nExecStart=\nExecStart="+exe+" run --config "+cfg+" --log-level debug\n"), 0o644)
	content, dropIns, err = readUnit(unitPath)
	if c := serviceUnitCheck(unitPath, content, err, dropIns, exe, cfg); c.Status != "ok" {
		t.Fatalf("current override: %+v", c)
	}
}

func fixHome(t *testing.T) (string, string) {
	t.Helper()
	home := filepath.Join(shortDir(t), ".siptooth")
	return home, filepath.Join(home, "config.yaml")
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestDoctorRoutingNeedsPrivilege(t *testing.T) {
	// The routing check must never say ok when it could not read the controller.
	c := routingCheck(func() (uint8, error) { return 0, os.ErrPermission })
	if c.Status != "warn" || !strings.Contains(c.Detail, "cannot verify routing without CAP_NET_RAW (run the probe once as root or check bluez-alsa docs)") || strings.Contains(c.Remedy, "sudo siptooth") {
		t.Fatalf("%+v", c)
	}
	if c := routingCheck(func() (uint8, error) { return 1, nil }); c.Status != "ok" {
		t.Fatalf("%+v", c)
	}
	if c := routingCheck(func() (uint8, error) { return 0, nil }); c.Status != "fail" {
		t.Fatalf("%+v", c)
	}
}

func TestParseBlueZVersion(t *testing.T) {
	cases := []struct {
		in           string
		major, minor int
		ok           bool
	}{
		{"bluetoothctl: 5.82\n", 5, 82, true},
		{"5.66", 5, 66, true},
		{"bluetoothctl: 5.84.1", 5, 84, true},
		{"garbage", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		maj, min, ok := parseBlueZVersion(c.in)
		if maj != c.major || min != c.minor || ok != c.ok {
			t.Errorf("%q: got %d.%d %v", c.in, maj, min, ok)
		}
	}
	if blueZVersionCheck("bluetoothctl: 5.65", "").Status != "fail" {
		t.Error("5.65 must fail")
	}
	if blueZVersionCheck("bluetoothctl: 5.82", "[General]\nExperimental = true\n").Status != "ok" {
		t.Error("5.82 ignores Experimental")
	}
	if c := blueZVersionCheck("bluetoothctl: 5.84", "[General]\n#Experimental = true\n"); c.Status != "ok" {
		t.Errorf("commented Experimental: %+v", c)
	}
	if c := blueZVersionCheck("bluetoothctl: 5.84", "[General]\nExperimental=True\n"); c.Status != "warn" || c.Remedy == "" {
		t.Errorf("5.84 with Experimental: %+v", c)
	}
	if c := blueZVersionCheck("bluetoothctl: 6.0", ""); c.Status != "ok" {
		t.Errorf("6.0: %+v", c)
	}
}

func TestCompetingHandlers(t *testing.T) {
	proc := t.TempDir()
	write := func(pid, comm string) {
		os.MkdirAll(filepath.Join(proc, pid), 0o755)
		os.WriteFile(filepath.Join(proc, pid, "comm"), []byte(comm+"\n"), 0o644)
	}
	write("1", "systemd")
	write("42", "bluetoothd")
	if c := competingHandlersCheck(proc); c.Status != "ok" {
		t.Fatalf("%+v", c)
	}
	write("100", "pipewire")
	write("101", "pipewire")
	write("102", "ofonod")
	c := competingHandlersCheck(proc)
	if c.Status != "fail" || c.Detail != "running: ofonod, pipewire" || c.Remedy == "" {
		t.Fatalf("%+v", c)
	}
}

// K: an unreadable manufacturer is reported, never treated as a non-Broadcom adapter.
func TestAdapterRoutingCheck(t *testing.T) {
	hci := func(v uint8) func() (uint8, error) { return func() (uint8, error) { return v, nil } }
	if c := adapterRoutingCheck(15, nil, hci(1)); c.Status != "ok" {
		t.Fatalf("%+v", c)
	}
	if c := adapterRoutingCheck(15, nil, hci(0)); c.Status != "fail" {
		t.Fatalf("%+v", c)
	}
	if c := adapterRoutingCheck(2, nil, hci(0)); c.Status != "skip" {
		t.Fatalf("%+v", c)
	}
	c := adapterRoutingCheck(0, errors.New("no such property"), hci(1))
	if c.Status != "warn" || !strings.Contains(c.Detail, "unknown") || c.Remedy == "" || strings.Contains(c.Remedy, "sudo") {
		t.Fatalf("%+v", c)
	}
	if c := adapterRoutingCheck(15, nil, func() (uint8, error) { return 0, os.ErrPermission }); c.Status != "warn" || !strings.Contains(c.Detail, "CAP_NET_RAW") {
		t.Fatalf("broadcom without capability: %+v", c)
	}
}

func TestCurrentIdentity(t *testing.T) {
	id, err := CurrentIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if id.Username == "" || id.UID != os.Getuid() || !slicesContains(id.Groups, os.Getgid()) {
		t.Fatalf("%+v", id)
	}
	if id.BluetoothGID < 0 && (id.InGroupFile || id.InBluetoothGroup()) {
		t.Fatalf("no group but membership: %+v", id)
	}
}

func slicesContains(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// --- siptooth-specific checks ---------------------------------------------------------

func writeConfig(t *testing.T, dir, extra string) string {
	t.Helper()
	p := filepath.Join(dir, "config.yaml")
	body := "bluetooth: {phone_address: 'AA:BB:CC:DD:EE:FF'}\nphone: {number: '+380441234567'}\nsip: {target: 'sip:{number}@127.0.0.1:5061;transport=tls'}\n" + extra
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDoctorDetectsAllochka(t *testing.T) {
	// FakeRunner with a nil error is `systemctl --user is-active --quiet allochka` exiting 0.
	r := &host.FakeRunner{}
	c := allochkaCheck(context.Background(), r)
	if c.Status != "fail" || !strings.Contains(c.Detail, "allochka") {
		t.Fatalf("active: %+v", c)
	}
	if len(r.Cmds) != 1 || r.Cmds[0].String() != "systemctl --user is-active --quiet allochka" {
		t.Fatalf("command: %v", r.Cmds)
	}
	if c := allochkaCheck(context.Background(), &host.FakeRunner{Err: errors.New("exit status 3")}); c.Status != "ok" {
		t.Fatalf("inactive: %+v", c)
	}
}

func TestPasswordCheck(t *testing.T) {
	cfg := config.Default()
	if c := passwordCheck(cfg, "", nil, "/e"); c.Status != "skip" {
		t.Fatalf("no username: %+v", c)
	}
	cfg.SIP.Username = "u"
	if c := passwordCheck(cfg, "", nil, "/e"); c.Status != "fail" || !strings.Contains(c.Detail, config.PasswordVar) {
		t.Fatalf("missing: %+v", c)
	}
	if c := passwordCheck(cfg, "secret", nil, "/e"); c.Status != "ok" || strings.Contains(c.Detail, "secret") {
		t.Fatalf("present: %+v", c)
	}
}

func TestRunReportsConfigEnvAndPassword(t *testing.T) {
	dir := shortDir(t)
	cfg := writeConfig(t, dir, "")
	t.Setenv(config.PasswordVar, "")
	checks := Run(context.Background(), Options{ConfigPath: cfg, NoNetwork: true, SkipPlatform: true, Identity: member,
		Runner: &host.FakeRunner{Err: errors.New("inactive")}, UnitPath: filepath.Join(dir, "none.service"), Executable: "/bin/siptooth"})
	for _, name := range []string{"config", "control_socket", "allochka_service"} {
		if c := find(checks, name); c.Status != "ok" {
			t.Errorf("%+v", c)
		}
	}
	if c := find(checks, "env_file"); c.Status != "warn" {
		t.Errorf("env: %+v", c)
	}
	if c := find(checks, "sip_password"); c.Status != "skip" {
		t.Errorf("password: %+v", c)
	}
	if find(checks, "dns").Status != "missing" {
		t.Error("network checks ran with NoNetwork")
	}
	// A config with a username and no password fails the password check and the config check.
	cfg = writeConfig(t, dir, "")
	os.WriteFile(cfg, append([]byte("sip: {username: bob, target: 'sip:x@127.0.0.1;transport=tls'}\nbluetooth: {phone_address: 'AA:BB:CC:DD:EE:FF'}\n"), nil...), 0o600)
	checks = Run(context.Background(), Options{ConfigPath: cfg, NoNetwork: true, SkipPlatform: true, Identity: member, Runner: &host.FakeRunner{Err: errors.New("x")}})
	if find(checks, "sip_password").Status != "fail" || find(checks, "config").Status != "fail" {
		t.Errorf("%+v", checks)
	}
}

func TestRunMissingConfigShortCircuits(t *testing.T) {
	checks := Run(context.Background(), Options{ConfigPath: filepath.Join(shortDir(t), "config.yaml"), NoNetwork: true, SkipPlatform: true, Identity: member})
	if len(checks) != 1 || checks[0].Status != "fail" || !strings.Contains(checks[0].Remedy, "doctor --fix") {
		t.Fatalf("%+v", checks)
	}
}

func TestDoctorTLSCheck(t *testing.T) {
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = c.(*tls.Conn).Handshake(); c.Close() }()
		}
	}()
	// Untrusted certificate: reachable but does not verify -> fail.
	c := tlsCheck(context.Background(), ln.Addr().String(), "localhost", time.Second, nil)
	if c.Status != "fail" || !strings.Contains(c.Detail, "does not verify") {
		t.Fatalf("untrusted: %+v", c)
	}
	// Trusting the certificate makes the same server pass.
	pool := x509.NewCertPool()
	selfSigned(t)
	pool.AddCert(serverLeaf)
	if c := tlsCheck(context.Background(), ln.Addr().String(), "localhost", time.Second, pool); c.Status != "ok" {
		t.Fatalf("trusted: %+v", c)
	}
	// Nothing listens: unreachable is a warning, not a failure.
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := closed.Addr().String()
	closed.Close()
	if c := tlsCheck(context.Background(), addr, "localhost", time.Second, nil); c.Status != "warn" {
		t.Fatalf("refused: %+v", c)
	}
}

var serverLeaf *x509.Certificate

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	if serverLeaf != nil && serverCert.Certificate != nil {
		return serverCert
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	serverLeaf, _ = x509.ParseCertificate(der)
	serverCert = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	return serverCert
}

var serverCert tls.Certificate

// fakeSTUN answers every Binding request with a mapped address on a fixed port.
func fakeSTUN(t *testing.T, mappedPort uint16) string {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < 20 {
				continue
			}
			b := []byte{0x01, 0x01, 0x00, 0x0c, 0x21, 0x12, 0xa4, 0x42}
			b = append(b, buf[8:20]...)
			b = append(b, 0x00, 0x20, 0x00, 0x08, 0x00, 0x01)
			var port [2]byte
			binary.BigEndian.PutUint16(port[:], mappedPort^0x2112)
			b = append(b, port[:]...)
			ip := netip.MustParseAddr("203.0.113.9").As4()
			cookie := []byte{0x21, 0x12, 0xa4, 0x42}
			for i := range ip {
				b = append(b, ip[i]^cookie[i])
			}
			pc.WriteTo(b, from)
		}
	}()
	return pc.LocalAddr().String()
}

func TestDoctorNATObservation(t *testing.T) {
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	same := natObservation(conn, []string{fakeSTUN(t, 40000), fakeSTUN(t, 40000)}, time.Second)
	if same.Status != "ok" || !strings.Contains(same.Detail, "endpoint-independent mapping observed") {
		t.Fatalf("same: %+v", same)
	}
	diff := natObservation(conn, []string{fakeSTUN(t, 40000), fakeSTUN(t, 40001)}, time.Second)
	if diff.Status != "warn" || !strings.Contains(diff.Detail, "address-dependent mapping: needs a latching far end") {
		t.Fatalf("different: %+v", diff)
	}
	// An unreachable STUN server is a warning, never a failure.
	dead, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	deadAddr := dead.LocalAddr().String()
	dead.Close()
	if c := natObservation(conn, []string{deadAddr, deadAddr}, 100*time.Millisecond); c.Status != "warn" {
		t.Fatalf("dead: %+v", c)
	}
}

func TestNextHop(t *testing.T) {
	cfg := config.Default()
	cfg.Phone.Number = "+100"
	for _, c := range []struct{ target, proxy, transport, want string }{
		{"sip:{number}@sip.example.com;transport=tls", "", "tls", "sip.example.com:5061"},
		{"sip:x@sip.example.com", "", "udp", "sip.example.com:5060"},
		{"sip:x@sip.example.com:5070;transport=tcp", "", "tcp", "sip.example.com:5070"},
		{"sip:x@sip.example.com", "proxy.example.com:5080", "udp", "proxy.example.com:5080"},
		{"sip:x@192.0.2.9", "", "udp", "192.0.2.9:5060"},
		{"sip:x@[2001:db8::1]:5060", "", "tcp", "[2001:db8::1]:5060"},
	} {
		cfg.SIP.Target, cfg.SIP.Proxy, cfg.SIP.Transport = c.target, c.proxy, c.transport
		got, _, err := nextHop(cfg)
		if err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.target, got, err, c.want)
		}
		// Doctor must test the address the daemon uses (both go through sipaddr).
		if d, err := sipaddr.NextHop(cfg); err != nil || d != got {
			t.Errorf("%s: daemon hop %q (%v) differs from doctor %q", c.target, d, err, got)
		}
	}
}

func TestOptionsCheckReportsReceivedRport(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	cfg := config.Default()
	cfg.SIP.Transport = "udp"
	c := optionsCheck(context.Background(), cfg, "", tr.HostPort(), "sip:x@127.0.0.1", nil)
	if c.Status != "ok" || !strings.Contains(c.Detail, "127.0.0.1:") {
		t.Fatalf("%+v", c)
	}
}

func TestDNSCheck(t *testing.T) {
	c, addr := dnsCheck(context.Background(), "127.0.0.1", nil)
	if c.Status != "ok" || addr != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("%+v %v", c, addr)
	}
	r := siptest.NewResolver(t, map[string]netip.Addr{"sip.test": netip.MustParseAddr("127.0.0.7")})
	if c, addr := dnsCheck(context.Background(), "sip.test", r); c.Status != "ok" || addr.String() != "127.0.0.7" {
		t.Fatalf("%+v %v", c, addr)
	}
	if c, addr := dnsCheck(context.Background(), "nope.test", r); c.Status != "warn" || addr.IsValid() {
		t.Fatalf("unresolvable must warn: %+v", c)
	}
}

func TestFixWritesEnvMode0600(t *testing.T) {
	home, cfg := fixHome(t)
	var out bytes.Buffer
	if err := Fix(context.Background(), FixOptions{ConfigPath: cfg, Identity: member, Out: &out}); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(home, "env")
	b, _ := os.ReadFile(env)
	if mode(t, env) != 0o600 || string(b) != config.PlaceholderEnvFile() || !strings.Contains(string(b), config.PasswordVar+"=") {
		t.Fatalf("env %o %q", mode(t, env), b)
	}
	if mode(t, home) != 0o700 || mode(t, cfg) != 0o600 {
		t.Fatalf("modes %o %o", mode(t, home), mode(t, cfg))
	}
	if got, _ := os.ReadFile(cfg); !bytes.Equal(got, config.DefaultYAML) {
		t.Fatal("config is not the default YAML")
	}
	// Idempotent and never overwrites; a loose env file is tightened.
	os.WriteFile(env, []byte("SIPTOOTH_SIP_PASSWORD=mine\n"), 0o644)
	os.Chmod(env, 0o644)
	os.WriteFile(cfg, []byte("mine: 1\n"), 0o600)
	out.Reset()
	if err := Fix(context.Background(), FixOptions{ConfigPath: cfg, Identity: member, Out: &out}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(env); string(b) != "SIPTOOTH_SIP_PASSWORD=mine\n" || mode(t, env) != 0o600 {
		t.Fatalf("env %q %o", b, mode(t, env))
	}
	if b, _ := os.ReadFile(cfg); string(b) != "mine: 1\n" {
		t.Fatalf("config overwritten: %s", b)
	}
}

func TestFixNeverRunsUsermod(t *testing.T) {
	_, cfg := fixHome(t)
	notMember := &Identity{Username: "pi", UID: 1000, Groups: []int{1000}, BluetoothGID: 112}
	var out bytes.Buffer
	if err := Fix(context.Background(), FixOptions{ConfigPath: cfg, Identity: notMember, Out: &out}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "sudo usermod -aG bluetooth pi") || strings.Contains(out.String(), "$ sudo") {
		t.Fatalf("%s", out.String())
	}
}
