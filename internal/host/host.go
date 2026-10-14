// Package host runs the few system commands siptooth needs (systemctl --user, loginctl
// enable-linger; never sudo) through an injectable Runner, and renders and parses the
// siptooth.service systemd user unit. Tests use FakeRunner and never touch the real system.
package host

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
)

// UserUnitPath is where `siptooth install` writes the systemd user unit:
// $XDG_CONFIG_HOME/systemd/user/siptooth.service, by default under ~/.config.
func UserUnitPath(home, xdgConfigHome string) string {
	if xdgConfigHome == "" {
		xdgConfigHome = filepath.Join(home, ".config")
	}
	return filepath.Join(xdgConfigHome, "systemd", "user", "siptooth.service")
}

// Cmd is one command line. Quiet discards its standard output. A failed Optional command
// is reported as a warning and does not stop the sequence.
type Cmd struct {
	Argv     []string
	Stdin    []byte
	Quiet    bool
	Optional bool
}

func (c Cmd) String() string {
	parts := make([]string, len(c.Argv))
	for i, a := range c.Argv {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`;&|<>()*?[]{}~#!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Runner executes commands.
type Runner interface {
	Run(ctx context.Context, c Cmd) error
}

// ExecRunner runs commands on this machine, with stdout and stderr connected to Out and Err.
type ExecRunner struct{ Out, Err io.Writer }

func (r ExecRunner) Run(ctx context.Context, c Cmd) error {
	if len(c.Argv) == 0 {
		return fmt.Errorf("empty command")
	}
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	if !c.Quiet {
		cmd.Stdout = r.Out
	}
	cmd.Stderr = r.Err
	return cmd.Run()
}

// FakeRunner records commands instead of running them. Fail maps a command line
// (Cmd.String) to its error; Err is returned from every other Run.
type FakeRunner struct {
	Cmds []Cmd
	Err  error
	Fail map[string]error
}

func (f *FakeRunner) Run(_ context.Context, c Cmd) error {
	f.Cmds = append(f.Cmds, c)
	if err, found := f.Fail[c.String()]; found {
		return err
	}
	return f.Err
}

// Do prints "$ command" to out, then runs it.
func Do(ctx context.Context, r Runner, out io.Writer, c Cmd) error {
	fmt.Fprintf(out, "$ %s\n", c)
	if err := r.Run(ctx, c); err != nil {
		return fmt.Errorf("%s: %w", c, err)
	}
	return nil
}

// UnitParams fills the unit template.
type UnitParams struct {
	ConfigPath string // absolute
	Executable string // absolute, symlinks resolved
}

// unitArg quotes a path for ExecStart when it contains whitespace or quotes.
func unitArg(s string) string {
	if !strings.ContainsAny(s, " \t\"'\\") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// Unit renders the siptooth.service user unit. It runs as the user who owns the systemd
// user manager, with that user's groups (bluetooth among them); no capabilities are
// granted. The SIP password is read by `siptooth run` from the env file next to the config.
func Unit(p UnitParams) string {
	return fmt.Sprintf(`[Unit]
Description=Siptooth phone-to-SIP bridge

[Service]
WorkingDirectory=%s
ExecStart=%s run --config %s
Restart=on-failure
RestartSec=3
RestartPreventExitStatus=2

[Install]
WantedBy=default.target
`, filepath.Dir(p.ConfigPath), unitArg(p.Executable), unitArg(p.ConfigPath))
}

// UnitInfo is what doctor compares against this binary and config.
type UnitInfo struct {
	Executable string // first word of ExecStart, without systemd prefixes
	Config     string // value of --config in ExecStart

	SetsExecStart bool // the content assigns ExecStart= at least once (drop-in detection)
}

// ParseUnit extracts ExecStart= (the last assignment wins; comments are skipped).
func ParseUnit(content string) UnitInfo {
	var info UnitInfo
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' || line[0] == '[' {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "ExecStart":
			info.SetsExecStart = true
			words := splitUnitWords(value)
			info.Executable, info.Config = "", ""
			if len(words) > 0 {
				info.Executable = strings.TrimLeft(words[0], "-@:+!")
			}
			for i := 1; i+1 < len(words); i++ {
				if words[i] == "--config" {
					info.Config = words[i+1]
				}
			}
		}
	}
	return info
}

// splitUnitWords splits on whitespace, honouring double quotes and backslash escapes.
func splitUnitWords(s string) []string {
	var out []string
	var cur strings.Builder
	inWord, quoted := false, false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inWord = true
		case ch == '"':
			quoted = !quoted
			inWord = true
		case (ch == ' ' || ch == '\t') && !quoted:
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(ch)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

// InstallCommands reloads the user manager, enables and restarts the service (restart
// starts a stopped service and refreshes a running one), then enables lingering so the
// user manager, and with it the service, runs from boot without a login. Lingering is
// optional: without it the service only runs while the user is logged in.
func InstallCommands(user string) []Cmd {
	return []Cmd{
		{Argv: []string{"systemctl", "--user", "daemon-reload"}},
		{Argv: []string{"systemctl", "--user", "enable", "siptooth"}},
		{Argv: []string{"systemctl", "--user", "restart", "siptooth"}},
		{Argv: []string{"loginctl", "enable-linger", user}, Optional: true},
	}
}
