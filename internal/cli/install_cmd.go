package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/host"
)

func init() {
	register(command{"install", "Install and (re)start the systemd user service (no sudo)", runInstall})
}

// Seams replaced by tests.
var (
	newRunner = func(stdout, stderr io.Writer) host.Runner { return host.ExecRunner{Out: stdout, Err: stderr} }

	// installIdentity returns the current user name and the euid.
	installIdentity = func() (string, int, error) {
		u, err := user.Current()
		if err != nil {
			return "", 0, err
		}
		return u.Username, os.Geteuid(), nil
	}

	installExecutable = currentExecutable
)

// rootRefusal is printed by the commands that set things up for the current user
// (install, doctor --fix) when run as root.
const rootRefusal = "run as your normal user, not as root or with sudo: siptooth needs no sudo"

// currentExecutable is the absolute path of this binary with symlinks resolved.
func currentExecutable() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// userUnitPath is the systemd user unit path for the current $HOME and $XDG_CONFIG_HOME.
func userUnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return host.UserUnitPath(home, os.Getenv("XDG_CONFIG_HOME")), nil
}

func runInstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfigPath(), "configuration file the service runs with")
	dryRun := fs.Bool("dry-run", false, "print the unit and the commands without running them")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	name, euid, err := installIdentity()
	if err != nil {
		fmt.Fprintln(stderr, "current user:", err)
		return ExitError
	}
	if euid == 0 {
		fmt.Fprintln(stderr, "siptooth install:", rootRefusal)
		return ExitUsage
	}
	if _, err := config.Load(*cfgPath); err != nil {
		fmt.Fprintln(stderr, err)
		fmt.Fprintln(stderr, "fix the configuration first: run `siptooth doctor --fix`, edit the file, then `siptooth config check`")
		return ExitUsage
	}
	absCfg, err := filepath.Abs(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return ExitError
	}
	exe, err := installExecutable()
	if err != nil {
		fmt.Fprintln(stderr, "locate executable:", err)
		return ExitError
	}
	unitPath, err := userUnitPath()
	if err != nil {
		fmt.Fprintln(stderr, "home directory:", err)
		return ExitError
	}
	unit := host.Unit(host.UnitParams{ConfigPath: absCfg, Executable: exe})
	cmds := host.InstallCommands(name)
	if *dryRun {
		fmt.Fprintf(stdout, "# %s\n%s\n", unitPath, unit)
		for _, c := range cmds {
			fmt.Fprintf(stdout, "$ %s\n", c)
		}
		return ExitOK
	}
	if !installSupported {
		fmt.Fprintln(stderr, "siptooth install is only supported on Linux with systemd (use --dry-run to see the unit)")
		return ExitError
	}
	fmt.Fprintf(stdout, "write %s\n", unitPath)
	if err := writeFileAtomic(unitPath, []byte(unit), 0o644); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitError
	}
	r := newRunner(stdout, stderr)
	for _, c := range cmds {
		if err := host.Do(context.Background(), r, stdout, c); err != nil {
			if c.Optional {
				fmt.Fprintf(stderr, "warning: %v\n  without lingering the service only runs while you are logged in (`siptooth doctor` reports it as linger)\n  it may need administrator rights over SSH; an administrator can run: sudo loginctl enable-linger %s\n", err, name)
				continue
			}
			fmt.Fprintln(stderr, err)
			if c.Argv[0] == "systemctl" && (os.Getenv("XDG_RUNTIME_DIR") == "" || strings.Contains(err.Error(), "bus")) {
				fmt.Fprintln(stderr, "hint: systemctl --user cannot reach your user manager (no session bus, XDG_RUNTIME_DIR unset, as after su or sudo -u); log in directly as the user (for example over ssh) and re-run `siptooth install`")
			}
			return ExitError
		}
	}
	fmt.Fprintf(stdout, "installed %s (ExecStart=%s run --config %s)\nlogs: journalctl --user -u siptooth -f\n", unitPath, exe, absCfg)
	return ExitOK
}

// writeFileAtomic writes data to a temporary file next to path and renames it into place,
// creating the parent directories.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".siptooth.service.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
