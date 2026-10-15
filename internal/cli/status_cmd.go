package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/bridge"
	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/control"
	"github.com/volodymyrsmirnov/siptooth/internal/sms"
)

func init() { register(command{"status", "Show daemon status", runStatus}) }

func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfigPath(), "configuration file")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		loadErr(stderr, err)
		return ExitError
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := control.NewClient(cfg.Control.Socket).Status(ctx)
	if errors.Is(err, control.ErrUnavailable) {
		fmt.Fprintf(stderr, "siptooth is not running (no daemon on %s)\n", cfg.Control.Socket)
		return ExitError
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return ExitError
	}
	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(st); err != nil {
			fmt.Fprintln(stderr, err)
			return ExitError
		}
		return ExitOK
	}
	printStatus(stdout, st, time.Now())
	return ExitOK
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func printStatus(w io.Writer, st control.Status, now time.Time) {
	fmt.Fprintf(w, "ready:           %s\nphone connected: %s\nstate:           %s\n", yesNo(st.Ready), yesNo(st.PhoneConnected), st.State)
	if st.CallID != "" {
		caller := st.Caller
		if caller == "" {
			caller = "withheld"
		}
		fmt.Fprintf(w, "call:            %s from %s", st.CallID, caller)
		if st.CallStartedAt != nil {
			fmt.Fprintf(w, " (%s)", now.Sub(*st.CallStartedAt).Round(time.Second))
		}
		fmt.Fprintln(w)
	}
	if st.LastOutcome != "" {
		fmt.Fprintf(w, "last call:       %s\n", st.LastOutcome)
	}
	printRegistration(w, st.Registration)
	printSMS(w, st.SMS)
}

func printRegistration(w io.Writer, r bridge.RegistrationStatus) {
	if !r.Enabled {
		fmt.Fprintln(w, "registration:    disabled")
		return
	}
	fmt.Fprintf(w, "registration:    registered=%s", yesNo(r.Registered))
	if r.Contact != "" {
		fmt.Fprintf(w, " contact=%s", r.Contact)
	}
	if r.Expires != nil {
		fmt.Fprintf(w, " expires=%s", r.Expires.Format(time.RFC3339))
	}
	if r.Error != "" {
		fmt.Fprintf(w, " error=%s", r.Error)
	}
	fmt.Fprintln(w)
}

// printSMS prints the SMS service lines (spec 17.10); they do not depend on the SIP registration.
func printSMS(w io.Writer, s sms.Status) {
	if !s.Enabled {
		fmt.Fprintln(w, "sms:             disabled")
		return
	}
	fmt.Fprintf(w, "sms:             %s", s.State)
	if s.Notifications != nil {
		fmt.Fprintf(w, ", notifications %s", yesNo(*s.Notifications))
	}
	if s.Received != nil {
		fmt.Fprintf(w, ", %d received", *s.Received)
	}
	fmt.Fprintln(w)
	if s.LastError != "" {
		fmt.Fprintf(w, "sms error:       %s\n", s.LastError)
	}
}
