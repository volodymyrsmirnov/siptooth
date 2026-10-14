package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/volodymyrsmirnov/siptooth/internal/config"
)

// FixOptions drives doctor --fix. Fix runs no external commands and never uses sudo.
type FixOptions struct {
	ConfigPath string
	Identity   *Identity
	Out        io.Writer
}

// Fix creates what is missing, printing each action before doing it:
//   - the config directory (0700) and config.yaml (the commented default, never overwritten);
//   - the env file (0600) with an empty SIPTOOTH_SIP_PASSWORD= placeholder; an existing env
//     file only gets chmod 0600;
//   - when the user is not in group bluetooth, the one-time administrator command
//     (sudo usermod -aG bluetooth USER) is printed, never executed.
//
// It carries on after a failed step and returns the errors joined.
func Fix(ctx context.Context, o FixOptions) error {
	var errs []error
	step := func(err error) {
		if err != nil {
			fmt.Fprintln(o.Out, "  failed:", err)
			errs = append(errs, err)
		}
	}
	step(o.mkdir(filepath.Dir(o.ConfigPath), 0o700))
	step(o.writeNew(o.ConfigPath, config.DefaultYAML, 0o600))

	envPath := config.EnvFilePath(o.ConfigPath)
	if fi, err := os.Stat(envPath); err == nil {
		if fi.Mode().Perm() != 0o600 {
			fmt.Fprintf(o.Out, "chmod 0600 %s\n", envPath)
			step(os.Chmod(envPath, 0o600))
		}
	} else if errors.Is(err, os.ErrNotExist) {
		step(o.writeNew(envPath, []byte(config.PlaceholderEnvFile()), 0o600))
		fmt.Fprintf(o.Out, "  put the SIP password into %s if sip.username is set\n", envPath)
	} else {
		step(err)
	}

	o.groupHint()
	return errors.Join(errs...)
}

func (o FixOptions) groupHint() {
	id := o.Identity
	switch {
	case id == nil, id.UID == 0, id.InBluetoothGroup():
	case id.BluetoothGID < 0:
		fmt.Fprintln(o.Out, "group bluetooth does not exist: install bluez, then rerun `siptooth doctor --fix`")
	case id.InGroupFile:
		fmt.Fprintf(o.Out, "%s is already in group bluetooth: log out and back in (or run `newgrp bluetooth`) to use it\n", id.Username)
	default:
		fmt.Fprintf(o.Out, "%s is not in group bluetooth (needed for BlueZ). siptooth does not use sudo; run this once yourself:\n  %s\n", id.Username, GroupRemedy(id.Username))
	}
}

// mkdir creates dir (and parents) with mode perm when it does not exist yet.
func (o FixOptions) mkdir(dir string, perm os.FileMode) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	fmt.Fprintf(o.Out, "create %s (mode %04o)\n", dir, perm)
	if err := os.MkdirAll(dir, perm); err != nil {
		return err
	}
	return os.Chmod(dir, perm) // MkdirAll is subject to the umask
}

// writeNew writes path only when it does not exist (O_EXCL: never overwrites).
func (o FixOptions) writeNew(path string, data []byte, perm os.FileMode) error {
	if _, err := os.Lstat(path); err == nil {
		return nil
	}
	fmt.Fprintf(o.Out, "write %s (mode %04o)\n", path, perm)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, perm)
}
