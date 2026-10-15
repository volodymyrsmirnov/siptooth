package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/bridge"
	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/control"
	"github.com/volodymyrsmirnov/siptooth/internal/doctor"
	"github.com/volodymyrsmirnov/siptooth/internal/host"
	"github.com/volodymyrsmirnov/siptooth/internal/sms"
)

func runCLI(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestConfigCheckListsAllErrors(t *testing.T) {
	// No phone address, no target, a bad transport and username without password.
	t.Setenv(config.PasswordVar, "")
	p := writeTempConfig(t, "sip: {transport: bogus, username: bob}\n")
	code, out, errOut := runCLI("config", "check", "--config", p)
	if code != ExitError || out != "" {
		t.Fatalf("exit %d stdout %q", code, out)
	}
	lines := strings.Split(strings.TrimSpace(errOut), "\n")
	if len(lines) < 3 {
		t.Fatalf("want every error, got %q", errOut)
	}
	for i := 1; i < len(lines); i++ {
		if lines[i-1] > lines[i] {
			t.Fatalf("not sorted: %q", errOut)
		}
	}
	for _, want := range []string{"bluetooth.phone_address", "sip.target", "sip.transport", "SIPTOOTH_SIP_PASSWORD"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("missing %q in %q", want, errOut)
		}
	}
}

func TestConfigCheckRedactsPassword(t *testing.T) {
	p := writeTempConfig(t, validConfigBody+"hooks: []\n")
	if err := os.WriteFile(filepath.Join(filepath.Dir(p), "env"), []byte(config.PasswordVar+"=s3cret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.PasswordVar, "")
	code, out, errOut := runCLI("config", "check", "--config", p)
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.Contains(out+errOut, "s3cret-value") {
		t.Fatalf("password printed:\n%s", out)
	}
	if !strings.Contains(out, config.PasswordVar+": set") || !strings.Contains(out, "phone_address: AA:BB:CC:DD:EE:FF") {
		t.Fatalf("output:\n%s", out)
	}
	// Without a password anywhere it says missing.
	os.Remove(filepath.Join(filepath.Dir(p), "env"))
	_, out, _ = runCLI("config", "check", "--config", p)
	if !strings.Contains(out, config.PasswordVar+": missing") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestConfigUsage(t *testing.T) {
	if code, _, _ := runCLI("config"); code != ExitUsage {
		t.Fatalf("exit %d", code)
	}
	if code, _, errOut := runCLI("config", "check", "--config", filepath.Join(shortTempDir(t), "none.yaml")); code != ExitError || !strings.Contains(errOut, "doctor --fix") {
		t.Fatalf("exit %d %s", code, errOut)
	}
}

func TestHooksTestRunsMatching(t *testing.T) {
	dir := shortTempDir(t)
	matched, other := filepath.Join(dir, "matched.json"), filepath.Join(dir, "other.json")
	body := validConfigBody + "hooks:\n" +
		"  - {id: a, events: [call.ended], command: 'cat > " + matched + "'}\n" +
		"  - {id: b, events: [call.ringing], command: 'cat > " + other + "'}\n"
	p := writeTempConfig(t, body)
	code, out, errOut := runCLI("hooks", "test", "--event", "call.ended", "--config", p)
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	b, err := os.ReadFile(matched)
	if err != nil {
		t.Fatalf("matching hook did not run: %v", err)
	}
	if !strings.Contains(string(b), `"test":true`) || !strings.Contains(string(b), `"event":"call.ended"`) {
		t.Fatalf("payload %s", b)
	}
	if _, err := os.Stat(other); err == nil {
		t.Fatal("non-matching hook ran")
	}
	if !strings.Contains(out, "hook a") || strings.Contains(out, "hook b") {
		t.Fatalf("output %s", out)
	}
	// system.error has its own payload; a failing hook gives a non-zero exit.
	p = writeTempConfig(t, validConfigBody+"hooks:\n  - {id: f, events: [system.error], command: 'cat >/dev/null; exit 3'}\n")
	code, out, _ = runCLI("hooks", "test", "--event", "system.error", "--config", p)
	if code != ExitError || !strings.Contains(out, `"system.error"`) || !strings.Contains(out, "exit code 3") {
		t.Fatalf("exit %d: %s", code, out)
	}
	// No listener is not an error; a bad event is a usage error.
	code, out, _ = runCLI("hooks", "test", "--event", "call.connected", "--config", p)
	if code != ExitOK || !strings.Contains(out, "no hook listens") {
		t.Fatalf("exit %d: %s", code, out)
	}
	if code, _, _ := runCLI("hooks", "test", "--event", "nope", "--config", p); code != ExitUsage {
		t.Fatalf("exit %d", code)
	}
	if code, _, _ := runCLI("hooks"); code != ExitUsage {
		t.Fatalf("exit %d", code)
	}
}

func TestStatusNotRunning(t *testing.T) {
	p := writeTempConfig(t, "")
	code, out, errOut := runCLI("status", "--config", p)
	if code != ExitError || out != "" || !strings.Contains(errOut, "not running") {
		t.Fatalf("exit %d out %q err %q", code, out, errOut)
	}
}

func TestPrintStatus(t *testing.T) {
	started := time.Now().Add(-90 * time.Second)
	expires := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	var b bytes.Buffer
	printStatus(&b, control.Status{Status: bridge.Status{Ready: true, PhoneConnected: true, State: "bridged", CallID: "c1", Caller: "+100", CallStartedAt: &started,
		LastOutcome: "completed", Registration: bridge.RegistrationStatus{Enabled: true, Registered: true, Contact: "sip:x@1.2.3.4:5", Expires: &expires}}}, time.Now())
	for _, want := range []string{"ready:           yes", "state:           bridged", "c1 from +100 (1m30s)", "last call:       completed", "registered=yes contact=sip:x@1.2.3.4:5 expires=2026-10-14T12:00:00Z"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in\n%s", want, b.String())
		}
	}
	b.Reset()
	printStatus(&b, control.Status{Status: bridge.Status{State: "idle"}}, time.Now())
	if !strings.Contains(b.String(), "registration:    disabled") {
		t.Errorf("%s", b.String())
	}
}

func TestPrintStatusSMS(t *testing.T) {
	yes, no, n := true, false, 4
	cases := []struct {
		name string
		st   control.Status
		want []string
		not  []string
	}{
		{"no registration, sms disabled", control.Status{Status: bridge.Status{State: "idle"}, SMS: sms.Status{State: "disabled"}},
			[]string{"registration:    disabled\n", "sms:             disabled\n"}, []string{"sms error"}},
		{"no registration, sms ready", control.Status{Status: bridge.Status{State: "idle"}, SMS: sms.Status{Enabled: true, State: "ready", Notifications: &yes, Received: &n}},
			[]string{"registration:    disabled\n", "sms:             ready, notifications yes, 4 received\n"}, []string{"sms error"}},
		{"registration, sms retrying with error", control.Status{Status: bridge.Status{State: "idle", Registration: bridge.RegistrationStatus{Enabled: true, Registered: true}},
			SMS: sms.Status{Enabled: true, State: "retrying", Notifications: &no, Received: &n, LastError: "no MAS"}},
			[]string{"registered=yes", "sms:             retrying, notifications no, 4 received\n", "sms error:       no MAS\n"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var b bytes.Buffer
			printStatus(&b, c.st, time.Now())
			for _, w := range c.want {
				if !strings.Contains(b.String(), w) {
					t.Errorf("missing %q in\n%s", w, b.String())
				}
			}
			for _, w := range c.not {
				if strings.Contains(b.String(), w) {
					t.Errorf("unexpected %q in\n%s", w, b.String())
				}
			}
		})
	}
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	called := false
	old := daemonRun
	daemonRun = func(context.Context, config.Config, string, *slog.Logger) error { called = true; return nil }
	t.Cleanup(func() { daemonRun = old })
	t.Setenv(logLevelVar, "")
	code, _, errOut := runCLI("run", "--config", writeTempConfig(t, "bluetooth: {phone_address: ''}\n"))
	if code != ExitUsage || called || !strings.Contains(errOut, "bluetooth.phone_address") || !strings.Contains(errOut, "sip.target") {
		t.Fatalf("exit %d called %v: %s", code, called, errOut)
	}
	code, _, errOut = runCLI("run", "--config", filepath.Join(shortTempDir(t), "none.yaml"))
	if code != ExitUsage || !strings.Contains(errOut, "doctor --fix") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

func TestRunStartsDaemonWithPassword(t *testing.T) {
	var gotPassword, gotTarget string
	old := daemonRun
	daemonRun = func(_ context.Context, cfg config.Config, password string, _ *slog.Logger) error {
		gotPassword, gotTarget = password, cfg.SIP.Target
		return nil
	}
	t.Cleanup(func() { daemonRun = old })
	t.Setenv(logLevelVar, "debug")
	t.Setenv(config.PasswordVar, "pw")
	p := writeTempConfig(t, "")
	if code, _, errOut := runCLI("run", "--config", p); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if gotPassword != "pw" || !strings.Contains(gotTarget, "sip.example.com") {
		t.Fatalf("daemon got %q %q", gotPassword, gotTarget)
	}
	daemonRun = func(context.Context, config.Config, string, *slog.Logger) error { return errors.New("boom") }
	if code, _, errOut := runCLI("run", "--config", p); code != ExitError || !strings.Contains(errOut, "boom") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	// Another daemon owns the socket: ExitUsage, so systemd does not restart-loop (exit 2).
	daemonRun = func(context.Context, config.Config, string, *slog.Logger) error {
		return fmt.Errorf("start: %w", control.ErrAlreadyRunning)
	}
	if code, _, errOut := runCLI("run", "--config", p); code != ExitUsage || !strings.Contains(errOut, "another daemon") {
		t.Fatalf("already running: exit %d: %s", code, errOut)
	}
	t.Setenv(logLevelVar, "loud")
	if code, _, errOut := runCLI("run", "--config", p); code != ExitUsage || !strings.Contains(errOut, logLevelVar) {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

func TestLogLevelFromEnv(t *testing.T) {
	for in, want := range map[string]slog.Level{"": slog.LevelInfo, "debug": slog.LevelDebug, "WARN": slog.LevelWarn, "error": slog.LevelError} {
		t.Setenv(logLevelVar, in)
		if got, ok := logLevelFromEnv(); !ok || got != want {
			t.Errorf("%q: %v %v", in, got, ok)
		}
	}
}

func TestSetPhoneAddress(t *testing.T) {
	p := writeTempConfig(t, "bluetooth:\n  adapter: hci0\n  phone_address: \"\"   # fill\nsip: {}\n")
	if err := setPhoneAddress(p, "11:22:33:44:55:66"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `phone_address: "11:22:33:44:55:66"`) || !strings.Contains(string(b), "sip: {}") {
		t.Fatalf("%s", b)
	}
	noLine := writeTempConfig(t, "sip: {}\n")
	if err := setPhoneAddress(noLine, "x"); err == nil {
		t.Fatal("want error without a phone_address line")
	}
}

func TestPairWithoutBluetooth(t *testing.T) {
	// On macOS (and Linux CI without BlueZ) connecting fails; pair must say so and exit 1.
	if _, err := os.Stat("/var/run/dbus/system_bus_socket"); err == nil {
		t.Skip("system bus present")
	}
	code, _, errOut := runCLI("pair", "--config", writeTempConfig(t, ""))
	if code != ExitError || !strings.Contains(errOut, "bluetooth:") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

func TestDoctorCommand(t *testing.T) {
	oldOpts, oldEuid, oldID := doctorOptions, doctorEuid, doctorIdentity
	doctorOptions = func(o doctor.Options) doctor.Options {
		o.NoNetwork = true
		o.SkipPlatform = true
		o.Runner = &host.FakeRunner{Err: errors.New("inactive")}
		o.UnitPath = filepath.Join(shortTempDir(t), "none.service")
		return o
	}
	doctorEuid = func() int { return 1000 }
	doctorIdentity = func() *doctor.Identity {
		return &doctor.Identity{Username: "pi", UID: 1000, Groups: []int{1000, 112}, BluetoothGID: 112, InGroupFile: true}
	}
	t.Cleanup(func() { doctorOptions, doctorEuid, doctorIdentity = oldOpts, oldEuid, oldID })
	t.Setenv(config.PasswordVar, "")

	// --fix on an empty home creates the files; the default config does not validate yet.
	cfg := filepath.Join(shortTempDir(t), ".siptooth", "config.yaml")
	code, out, errOut := runCLI("doctor", "--fix", "--config", cfg)
	if code != ExitError {
		t.Fatalf("exit %d (default config must fail validation): %s %s", code, out, errOut)
	}
	fi, err := os.Stat(filepath.Join(filepath.Dir(cfg), "env"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("env: %v %v", fi, err)
	}
	if !strings.Contains(out, "fail  config") || !strings.Contains(out, "ok    allochka_service") {
		t.Fatalf("report:\n%s", out)
	}
	// A valid config passes; --json is machine readable.
	good := writeTempConfig(t, "")
	code, out, errOut = runCLI("doctor", "--json", "--config", good)
	var report []doctor.Check
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("json does not parse: %v\n%s %s", err, out, errOut)
	}
	if len(report) == 0 || report[0].Name != "config" || report[0].Status != "ok" {
		t.Fatalf("json report: %+v", report)
	}
	// Exit status follows failures; the platform checks are skipped, so only the report decides.
	wantCode := ExitOK
	if doctor.HasFailures(report) {
		wantCode = ExitError
	}
	if code != wantCode {
		t.Fatalf("exit %d, want %d", code, wantCode)
	}
	// --fix refuses root.
	doctorEuid = func() int { return 0 }
	if code, _, errOut := runCLI("doctor", "--fix", "--config", cfg); code != ExitUsage || !strings.Contains(errOut, "root") {
		t.Fatalf("root: %d %s", code, errOut)
	}
}

func TestHooksTestSMSSample(t *testing.T) {
	dir := shortTempDir(t)
	got := filepath.Join(dir, "sms.json")
	p := writeTempConfig(t, validConfigBody+"hooks:\n  - {id: s, events: [sms.received], command: 'cat > "+got+"; echo \"from=$SIPTOOTH_SMS_FROM call=[$SIPTOOTH_CALL_ID]\"'}\n")
	code, out, errOut := runCLI("hooks", "test", "--event", "sms.received", "--config", p)
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	b, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"event":"sms.received"`, `"test":true`, `"from":"+380501234567"`, `"from_name":"Test Sender"`, `"text":"Test message from siptooth"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("payload lacks %s: %s", want, b)
		}
	}
	if !strings.Contains(out, "from=+380501234567 call=[]") {
		t.Fatalf("hook output not printed: %s", out)
	}
}
