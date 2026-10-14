package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/hooks"
)

func init() {
	register(command{"hooks", "hooks test --event EVENT [--config PATH]", runHooks})
}

// sampleCall is the call every sample payload describes.
func sampleCall() hooks.CallInfo {
	caller := "+15551234567"
	connected := time.Now().UTC().Add(-30 * time.Second)
	bridged := 30.0
	return hooks.CallInfo{
		ID: "test-call", Number: "+15551234567", Target: "sip:test@example.com",
		Caller: &caller, SIPCallID: "test-sip-call-id@siptooth", Codec: "PCMU", NATMode: "local",
		RemoteMedia: "192.0.2.10:20000", AdvertisedMedia: "192.0.2.20:10000", ConnectedAt: &connected,
		Outcome: "completed", HangupBy: "far_end", SIPStatus: 200, RingingSeconds: 3.5, BridgedSeconds: &bridged,
		Media: &hooks.MediaStats{Sent: 1500, Received: 1498},
	}
}

func runHooks(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "test" {
		fmt.Fprintln(stderr, "usage: siptooth hooks test --event EVENT [--config PATH]")
		return ExitUsage
	}
	fs := flag.NewFlagSet("hooks test", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfigPath(), "configuration file")
	event := fs.String("event", "", "event name ("+strings.Join(config.KnownEvents, ", ")+")")
	if err := fs.Parse(args[1:]); err != nil {
		return ExitUsage
	}
	if !slices.Contains(config.KnownEvents, *event) {
		fmt.Fprintf(stderr, "--event is required and must be one of: %s\n", strings.Join(config.KnownEvents, ", "))
		return ExitUsage
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		loadErr(stderr, err)
		return ExitError
	}
	var payload []byte
	if *event == "system.error" {
		payload, err = hooks.BuildErrorPayload("test", "hooks test: sample system.error", true)
	} else {
		payload, err = hooks.BuildPayload(*event, sampleCall(), true)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return ExitError
	}
	callID, caller := "test-call", "+15551234567"
	if *event == "system.error" {
		callID, caller = "", ""
	}
	code := ExitOK
	matched := 0
	for _, h := range cfg.Hooks {
		if !slices.Contains(h.Events, *event) {
			continue
		}
		matched++
		fmt.Fprintf(stdout, "hook %s, event %s, command: %s\npayload:\n%s", h.ID, *event, h.Command, payload)
		res := hooks.Run(context.Background(), h.Command, time.Duration(h.TimeoutSeconds)*time.Second, payload, hooks.Env(*event, callID, caller))
		fmt.Fprintf(stdout, "exit code %d, timed out %v, duration %s\n--- stdout ---\n%s\n--- stderr ---\n%s\n",
			res.ExitCode, res.TimedOut, res.FinishedAt.Sub(res.StartedAt).Round(time.Millisecond), res.Stdout, res.Stderr)
		if res.ExitCode != 0 || res.TimedOut {
			code = ExitError
		}
	}
	if matched == 0 {
		fmt.Fprintf(stdout, "no hook listens for %s\n", *event)
	}
	return code
}
