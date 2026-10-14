package cli

import (
	"os"
	"path/filepath"
)

// homeDirName is the per-user directory holding config.yaml, env and the control socket.
const homeDirName = ".siptooth"

// configEnvVar overrides the default config path.
const configEnvVar = "SIPTOOTH_CONFIG"

// defaultConfigPath is ~/.siptooth/config.yaml, or $SIPTOOTH_CONFIG when set.
func defaultConfigPath() string {
	if p := os.Getenv(configEnvVar); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "." // no $HOME: fall back to the working directory
	}
	return filepath.Join(home, homeDirName, "config.yaml")
}

// configHint is appended to "cannot open config" errors.
const configHint = "run `siptooth doctor --fix` to create it"
