package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/volodymyrsmirnov/siptooth/internal/host"
)

// fakeInstallEnv replaces the identity, executable, platform and runner seams and points
// HOME at a temporary directory. It returns the runner and the expected unit path.
func fakeInstallEnv(t *testing.T, euid int, supported bool) (*host.FakeRunner, string) {
	t.Helper()
	home := shortTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	f := &host.FakeRunner{}
	oldID, oldExe, oldSup, oldRun := installIdentity, installExecutable, installSupported, newRunner
	installIdentity = func() (string, int, error) { return "pi", euid, nil }
	installExecutable = func() (string, error) { return "/home/pi/.local/bin/siptooth", nil }
	installSupported = supported
	newRunner = func(_, _ io.Writer) host.Runner { return f }
	t.Cleanup(func() {
		installIdentity, installExecutable, installSupported, newRunner = oldID, oldExe, oldSup, oldRun
	})
	return f, filepath.Join(home, ".config", "systemd", "user", "siptooth.service")
}

const wantInstallCmds = "systemctl --user daemon-reload|systemctl --user enable siptooth|systemctl --user restart siptooth|loginctl enable-linger pi"

func cmdLines(f *host.FakeRunner) string {
	var got []string
	for _, c := range f.Cmds {
		got = append(got, c.String())
	}
	return strings.Join(got, "|")
}

func TestInstallDryRunPrintsUnitAndCommands(t *testing.T) {
	f, unitPath := fakeInstallEnv(t, 1000, false) // dry run works on every platform
	p := writeTempConfig(t, "")
	var out, errOut bytes.Buffer
	if code := Run([]string{"install", "--config", p, "--dry-run"}, &out, &errOut); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	s := out.String()
	for _, want := range []string{
		"# " + unitPath + "\n[Unit]\nDescription=Siptooth phone-to-SIP bridge\n",
		"[Service]\nWorkingDirectory=" + filepath.Dir(p) + "\nExecStart=/home/pi/.local/bin/siptooth run --config " + p + "\nRestart=on-failure\nRestartSec=3\nRestartPreventExitStatus=2\n",
		"[Install]\nWantedBy=default.target\n",
		"$ systemctl --user daemon-reload\n$ systemctl --user enable siptooth\n$ systemctl --user restart siptooth\n$ loginctl enable-linger pi\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	for _, bad := range []string{"sudo", "User=", "AmbientCapabilities", "SupplementaryGroups"} {
		if strings.Contains(s, bad) {
			t.Errorf("dry run mentions %q:\n%s", bad, s)
		}
	}
	if len(f.Cmds) != 0 {
		t.Fatalf("dry run executed %v", f.Cmds)
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote the unit: %v", err)
	}
}

func TestInstallWritesUserUnitAndRunsCommands(t *testing.T) {
	f, unitPath := fakeInstallEnv(t, 1000, true)
	p := writeTempConfig(t, "")
	var out, errOut bytes.Buffer
	if code := Run([]string{"install", "--config", p}, &out, &errOut); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if got := cmdLines(f); got != wantInstallCmds {
		t.Fatalf("%q", got)
	}
	b, err := os.ReadFile(unitPath)
	if err != nil || !strings.Contains(string(b), "ExecStart=/home/pi/.local/bin/siptooth run --config "+p+"\n") {
		t.Fatalf("unit %v:\n%s", err, b)
	}
	if !strings.Contains(out.String(), "write "+unitPath) || !strings.Contains(out.String(), "$ systemctl --user restart siptooth") || !strings.Contains(out.String(), "journalctl --user-unit siptooth -f") {
		t.Fatalf("output: %s", out.String())
	}
	// Re-running refreshes: the unit is rewritten (here with another binary).
	installExecutable = func() (string, error) { return "/home/pi/bin/siptooth", nil }
	if code := Run([]string{"install", "--config", p}, &out, &errOut); code != ExitOK {
		t.Fatalf("rerun exit %d: %s", code, errOut.String())
	}
	if b, _ := os.ReadFile(unitPath); !strings.Contains(string(b), "ExecStart=/home/pi/bin/siptooth run") {
		t.Fatalf("not refreshed:\n%s", b)
	}
}

func TestInstallHonoursXDGConfigHome(t *testing.T) {
	fakeInstallEnv(t, 1000, true)
	xdg := shortTempDir(t)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	p := writeTempConfig(t, "")
	var out, errOut bytes.Buffer
	if code := Run([]string{"install", "--config", p}, &out, &errOut); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(xdg, "systemd", "user", "siptooth.service")); err != nil {
		t.Fatal(err)
	}
}

func TestInstallToleratesLingerFailure(t *testing.T) {
	f, _ := fakeInstallEnv(t, 1000, true)
	f.Fail = map[string]error{"loginctl enable-linger pi": errors.New("access denied")}
	p := writeTempConfig(t, "")
	var out, errOut bytes.Buffer
	if code := Run([]string{"install", "--config", p}, &out, &errOut); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if got := cmdLines(f); got != wantInstallCmds {
		t.Fatalf("%q", got)
	}
	e := errOut.String()
	if !strings.Contains(e, "warning") || !strings.Contains(e, "access denied") || !strings.Contains(e, "only runs while you are logged in") || !strings.Contains(e, "sudo loginctl enable-linger pi") {
		t.Fatalf("stderr %q", e)
	}
}

func TestInstallRefusesRoot(t *testing.T) {
	f, unitPath := fakeInstallEnv(t, 0, true)
	p := writeTempConfig(t, "")
	var out, errOut bytes.Buffer
	if code := Run([]string{"install", "--config", p}, &out, &errOut); code != ExitUsage {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut.String(), "run as your normal user") || len(f.Cmds) != 0 {
		t.Fatalf("%q %v", errOut.String(), f.Cmds)
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Fatalf("root wrote the unit: %v", err)
	}
}

func TestInstallRefusesInvalidConfig(t *testing.T) {
	f, _ := fakeInstallEnv(t, 1000, true)
	var out, errOut bytes.Buffer
	if code := Run([]string{"install", "--config", t.TempDir() + "/missing.yaml"}, &out, &errOut); code != ExitUsage {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut.String(), "doctor --fix") || len(f.Cmds) != 0 {
		t.Fatalf("%q %v", errOut.String(), f.Cmds)
	}
	p := writeTempConfig(t, t.TempDir()) // no scripts
	errOut.Reset()
	if code := Run([]string{"install", "--config", p}, &out, &errOut); code != ExitUsage || !strings.Contains(errOut.String(), "doctor --fix") {
		t.Fatalf("exit %d %q", code, errOut.String())
	}
}

func TestInstallUnsupportedPlatform(t *testing.T) {
	f, unitPath := fakeInstallEnv(t, 1000, false)
	p := writeTempConfig(t, "")
	var out, errOut bytes.Buffer
	if code := Run([]string{"install", "--config", p}, &out, &errOut); code != ExitError || !strings.Contains(errOut.String(), "Linux") || len(f.Cmds) != 0 {
		t.Fatalf("exit %d %q %v", code, errOut.String(), f.Cmds)
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Fatalf("wrote the unit: %v", err)
	}
}

func TestInstallSupportedOnlyOnLinux(t *testing.T) {
	if installSupported != (runtime.GOOS == "linux") {
		t.Fatalf("installSupported %v on %s", installSupported, runtime.GOOS)
	}
}

func TestInstallStopsOnCommandFailure(t *testing.T) {
	f, _ := fakeInstallEnv(t, 1000, true)
	f.Err = bytes.ErrTooLarge
	p := writeTempConfig(t, "")
	var out, errOut bytes.Buffer
	if code := Run([]string{"install", "--config", p}, &out, &errOut); code != ExitError || len(f.Cmds) != 1 {
		t.Fatalf("exit %d, %d commands", code, len(f.Cmds))
	}
}

func TestInstallHintsOnMissingUserBus(t *testing.T) {
	f, _ := fakeInstallEnv(t, 1000, true)
	t.Setenv("XDG_RUNTIME_DIR", "")
	f.Fail = map[string]error{"systemctl --user daemon-reload": errors.New("exit status 1")}
	p := writeTempConfig(t, "")
	var out, errOut bytes.Buffer
	if code := Run([]string{"install", "--config", p}, &out, &errOut); code != ExitError {
		t.Fatalf("exit %d", code)
	}
	if e := errOut.String(); !strings.Contains(e, "log in directly as the user") || strings.Contains(e, "sudo systemctl") {
		t.Fatalf("stderr %q", e)
	}
}
