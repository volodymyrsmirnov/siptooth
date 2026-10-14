package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/volodymyrsmirnov/siptooth/internal/doctor"
)

func init() {
	register(command{"doctor", "Check the setup for the current user (--fix creates what is missing)", runDoctor})
}

// doctorEuid and doctorIdentity are replaced by tests.
var doctorEuid = os.Geteuid

var doctorIdentity = func() *doctor.Identity {
	id, err := doctor.CurrentIdentity()
	if err != nil {
		return nil // doctor falls back to the uid only
	}
	return id
}

// doctorOptions lets tests keep doctor off the real system and network.
var doctorOptions = func(o doctor.Options) doctor.Options { return o }

func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfigPath(), "configuration file")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fix := fs.Bool("fix", false, "create missing files in the config directory, then check again (never uses sudo)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	ctx := context.Background()
	if *fix && doctorEuid() == 0 {
		fmt.Fprintln(stderr, "siptooth doctor --fix:", rootRefusal)
		return ExitUsage
	}
	if *fix {
		log := stdout
		if *asJSON {
			log = stderr
		}
		err := doctor.Fix(ctx, doctor.FixOptions{ConfigPath: *cfgPath, Identity: doctorIdentity(), Out: log})
		if err != nil {
			fmt.Fprintln(stderr, "doctor --fix:", err)
		}
		fmt.Fprintln(log)
	}
	checks := doctor.Run(ctx, doctorOptions(doctor.Options{ConfigPath: *cfgPath, Identity: doctorIdentity()}))
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(checks); err != nil {
			fmt.Fprintln(stderr, err)
			return ExitError
		}
	} else {
		for _, c := range checks {
			fmt.Fprintf(stdout, "%-5s %-18s %s\n", c.Status, c.Name, c.Detail)
			if c.Remedy != "" && c.Status != "ok" {
				fmt.Fprintf(stdout, "      -> %s\n", c.Remedy)
			}
		}
	}
	if doctor.HasFailures(checks) {
		return ExitError
	}
	return ExitOK
}
