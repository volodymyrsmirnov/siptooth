// Package cli implements the siptooth command line.
package cli

import (
	"fmt"
	"io"

	"github.com/volodymyrsmirnov/siptooth/internal/buildinfo"
)

// Exit codes.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

type command struct {
	name    string
	summary string
	run     func(args []string, stdout, stderr io.Writer) int
}

// commands is appended to by other files in this package (init functions).
var commands []command

func register(c command) { commands = append(commands, c) }

func init() {
	register(command{"version", "Print version", func(args []string, stdout, stderr io.Writer) int {
		fmt.Fprintf(stdout, "siptooth %s\n", buildinfo.Version)
		return ExitOK
	}})
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: siptooth <command> [flags]")
	fmt.Fprintln(w, "commands:")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-18s %s\n", c.name, c.summary)
	}
}

// Run dispatches args to a subcommand and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(stderr)
		return ExitUsage
	}
	for _, c := range commands {
		if c.name == args[0] {
			return c.run(args[1:], stdout, stderr)
		}
	}
	fmt.Fprintf(stderr, "unknown command %q\n", args[0])
	usage(stderr)
	return ExitUsage
}
