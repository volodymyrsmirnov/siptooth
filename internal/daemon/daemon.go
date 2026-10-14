// Package daemon is the composition root for `siptooth run` (spec 3): it owns
// the hook dispatcher, the SIP user agent and registration, the call
// coordinator, the control socket and the Bluetooth glue for the lifetime of
// the process.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	gosip "github.com/emiago/sipgo/sip"

	"github.com/volodymyrsmirnov/siptooth/internal/bridge"
	"github.com/volodymyrsmirnov/siptooth/internal/buildinfo"
	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/control"
	"github.com/volodymyrsmirnov/siptooth/internal/hooks"
	"github.com/volodymyrsmirnov/siptooth/internal/placeholder"
	"github.com/volodymyrsmirnov/siptooth/internal/sco"
	"github.com/volodymyrsmirnov/siptooth/internal/sip"
)

// coordinator is the part of *bridge.Coordinator the Bluetooth glue uses.
type coordinator interface {
	Attach(t bridge.Telephony, l sco.Listener, adapterAddress string)
	Detach()
	SetAudioRouting(ok bool)
	Status() bridge.Status
}

// errorSink receives system.error events (*hooks.Dispatcher).
type errorSink interface {
	EmitError(code, detail string)
}

const (
	// hooksDrainTimeout is how long shutdown lets the hook dispatcher deliver
	// the final events (the call.ended of a call cut short by shutdown).
	hooksDrainTimeout = 2 * time.Second
	// coordinatorStopTimeout bounds waiting for the coordinator to end the
	// call in progress (BYE and AT+CHUP are bounded to 3 s each).
	coordinatorStopTimeout = 8 * time.Second
)

// Run starts the daemon and blocks until ctx ends (nil) or a component fails
// (that error). Every goroutine it started has returned when it does.
func Run(ctx context.Context, cfg config.Config, password string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	// Exclusive ownership first: never start anything while another daemon is live.
	if err := control.Probe(cfg.Control.Socket); err != nil {
		return err
	}

	// Components are stopped in a fixed order at shutdown: the coordinator
	// first (it ends the call in progress, which needs Bluetooth and SIP),
	// then Bluetooth, the registration, the hook dispatcher (so the final
	// call.ended is delivered) and finally the control socket and the user
	// agent. Each stop is a no-op until its component started.
	nop := func() {}
	stopCoord, stopBT, stopReg, stopHooks, stopCtl, stopUA := nop, nop, nop, nop, nop, nop
	defer func() {
		stopCoord()
		stopBT()
		stopReg()
		stopHooks()
		stopCtl()
		stopUA()
	}()

	hookCtx, hookCancel := context.WithCancel(context.Background())
	dispatcher := hooks.NewDispatcher(cfg.Hooks, logger)
	hookDone := make(chan struct{})
	go func() { defer close(hookDone); dispatcher.Run(hookCtx) }()
	stopHooks = func() {
		deadline := time.Now().Add(hooksDrainTimeout)
		for dispatcher.Outstanding() > 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if n := dispatcher.Outstanding(); n > 0 {
			logger.Warn("daemon: hook events undelivered at shutdown", "count", n)
		}
		hookCancel()
		<-hookDone
	}

	nextHop, err := nextHopAddress(cfg)
	if err != nil {
		return err
	}
	ua, err := sip.NewUA(sip.Options{
		Transport: cfg.SIP.Transport,
		NextHop:   nextHop,
		LocalPort: cfg.SIP.LocalPort,
		Username:  cfg.SIP.Username,
		Password:  password,
		UserAgent: "siptooth/" + buildinfo.Version,
		Logger:    logger,
	})
	if err != nil {
		return err
	}
	stopUA = func() { _ = ua.Close() }

	var reg atomic.Pointer[sip.Registration]
	adapter := bridge.NewSIPAdapter(bridge.AdapterDeps{
		UA: ua, Config: cfg, Hooks: dispatcher, Logger: logger,
		Registration: func() (sip.RegState, bool) {
			r := reg.Load()
			if r == nil {
				return sip.RegState{}, false
			}
			return r.State(), true
		},
	})
	coord := bridge.New(bridge.Deps{
		Config: cfg, SIP: adapter, Dialer: sco.NewDialer(), Hooks: dispatcher, Logger: logger,
	})
	coordCtx, coordCancel := context.WithCancel(context.Background())
	coordDone := make(chan struct{})
	go func() { defer close(coordDone); _ = coord.Run(coordCtx) }()
	stopCoord = func() {
		coordCancel()
		select {
		case <-coordDone:
		case <-time.After(coordinatorStopTimeout):
			logger.Warn("daemon: coordinator did not stop in time")
		}
	}

	if cfg.SIP.Register {
		coord.SetRegistration(bridge.RegistrationStatus{Enabled: true})
		rep := &registrationReporter{
			user: cfg.SIP.Username, transport: strings.ToLower(cfg.SIP.Transport),
			status: coord.SetRegistration, errors: dispatcher, log: logger,
		}
		regCtx, regCancel := context.WithCancel(context.Background())
		reg.Store(ua.Register(regCtx, "", cfg.SIP.RegistrarHost(),
			time.Duration(cfg.SIP.RegisterExpiresSeconds)*time.Second, rep.onState))
		stopReg = func() {
			regCancel()
			uctx, cancel := context.WithTimeout(context.Background(), unregisterTimeout)
			defer cancel()
			if err := reg.Load().Unregister(uctx); err != nil {
				logger.Warn("sip: unregister", "err", err)
			}
		}
	}

	errCh := make(chan error, 1)
	ctlCtx, ctlCancel := context.WithCancel(context.Background())
	ctlDone := make(chan struct{})
	go func() {
		defer close(ctlDone)
		if err := control.Serve(ctlCtx, cfg.Control.Socket, control.NewServer(coord)); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- fmt.Errorf("control socket: %w", err)
		}
	}()
	stopCtl = func() { ctlCancel(); <-ctlDone }

	btCtx, btCancel := context.WithCancel(context.Background())
	btDone := make(chan struct{})
	go func() { defer close(btDone); runBluetooth(btCtx, cfg, coord, dispatcher, logger) }()
	stopBT = func() { btCancel(); <-btDone }

	logger.Info("daemon: started", "version", buildinfo.Version, "control", cfg.Control.Socket,
		"transport", cfg.SIP.Transport, "next_hop", nextHop, "register", cfg.SIP.Register)
	select {
	case <-ctx.Done():
		logger.Info("daemon: stopping")
		return nil
	case err := <-errCh:
		logger.Error("daemon: component failed, stopping", "err", err)
		return err
	}
}

// nextHopAddress is "host:port" of sip.proxy, else of the sip.target host
// (default port 5061 for TLS, else 5060).
func nextHopAddress(cfg config.Config) (string, error) {
	if cfg.SIP.Proxy != "" {
		return cfg.SIP.Proxy, nil
	}
	uri, err := placeholder.ExpandURI(cfg.SIP.Target, placeholder.Values{Caller: "caller", Number: cfg.Phone.Number, CallID: "call"})
	if err != nil {
		return "", fmt.Errorf("sip.target: %w", err)
	}
	var u gosip.Uri
	if err := gosip.ParseUri(uri, &u); err != nil || u.Host == "" {
		return "", fmt.Errorf("sip.target %q has no host", cfg.SIP.Target)
	}
	port := u.Port
	if port == 0 {
		port = 5060
		if strings.EqualFold(cfg.SIP.Transport, "tls") {
			port = 5061
		}
	}
	host := strings.Trim(u.Host, "[]")
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.String()
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}
