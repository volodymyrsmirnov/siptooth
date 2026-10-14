package cli

import (
	"flag"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"

	"github.com/volodymyrsmirnov/siptooth/internal/config"
)

func init() {
	register(command{"config", "config check [--config PATH]", runConfig})
}

func runConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "check" {
		fmt.Fprintln(stderr, "usage: siptooth config check [--config PATH]")
		return ExitUsage
	}
	fs := flag.NewFlagSet("config check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", defaultConfigPath(), "configuration file")
	if err := fs.Parse(args[1:]); err != nil {
		return ExitUsage
	}
	cfg, err := config.Load(*path)
	if err != nil {
		loadErr(stderr, err)
		return ExitError
	}
	password, err := config.LookupPassword(*path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return ExitError
	}
	if errs := config.Validate(cfg, password); len(errs) > 0 {
		for _, m := range sortedErrors(errs) {
			fmt.Fprintln(stderr, m)
		}
		return ExitError
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return ExitError
	}
	fmt.Fprintf(stdout, "# effective configuration of %s\n%s", *path, out)
	state := "missing"
	if password != "" {
		state = "set"
	}
	fmt.Fprintf(stdout, "# %s: %s\n", config.PasswordVar, state)
	return ExitOK
}
