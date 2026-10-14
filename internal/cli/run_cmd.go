package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"

	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/control"
	"github.com/volodymyrsmirnov/siptooth/internal/daemon"
)

func init() { register(command{"run", "Run the daemon in the foreground (systemd runs this)", runRun}) }

// daemonRun is replaced by tests.
var daemonRun = daemon.Run

// loadErr prints a config load error, pointing at doctor --fix when the file is missing.
func loadErr(stderr io.Writer, err error) {
	fmt.Fprintln(stderr, err)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(stderr, configHint)
	}
}

// sortedErrors returns the messages of errs, sorted.
func sortedErrors(errs []error) []string {
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.Error()
	}
	sort.Strings(msgs)
	return msgs
}

// loadChecked loads and validates the config at path. On failure it prints every problem to
// stderr and returns ok=false.
func loadChecked(path string, stderr io.Writer) (cfg config.Config, password string, ok bool) {
	cfg, err := config.Load(path)
	if err != nil {
		loadErr(stderr, err)
		return cfg, "", false
	}
	password, err = config.LookupPassword(path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return cfg, "", false
	}
	if errs := config.Validate(cfg, password); len(errs) > 0 {
		fmt.Fprintf(stderr, "%s is invalid:\n", path)
		for _, m := range sortedErrors(errs) {
			fmt.Fprintln(stderr, "  "+m)
		}
		return cfg, "", false
	}
	return cfg, password, true
}

// runRun: a bad config exits ExitUsage, which the systemd unit lists in
// RestartPreventExitStatus so a broken config does not restart in a loop; runtime
// failures exit ExitError and are restarted.
func runRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfigPath(), "configuration file")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	level, ok := logLevelFromEnv()
	if !ok {
		fmt.Fprintf(stderr, "invalid %s %q (debug, info, warn or error)\n", logLevelVar, os.Getenv(logLevelVar))
		return ExitUsage
	}
	cfg, password, ok := loadChecked(*cfgPath, stderr)
	if !ok {
		return ExitUsage
	}
	logger := newLogger(stderr, level)
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("siptooth starting", "config", *cfgPath)
	if err := daemonRun(ctx, cfg, password, logger); err != nil {
		fmt.Fprintln(stderr, err)
		if errors.Is(err, control.ErrAlreadyRunning) {
			return ExitUsage // systemd's RestartPreventExitStatus: restarting cannot help
		}
		return ExitError
	}
	logger.Info("siptooth stopped")
	return ExitOK
}
