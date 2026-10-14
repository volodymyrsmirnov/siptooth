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
	"strings"
	"sync/atomic"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/bridge"
	"github.com/volodymyrsmirnov/siptooth/internal/buildinfo"
	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/control"
	"github.com/volodymyrsmirnov/siptooth/internal/hooks"
	"github.com/volodymyrsmirnov/siptooth/internal/sco"
	"github.com/volodymyrsmirnov/siptooth/internal/sip"
	"github.com/volodymyrsmirnov/siptooth/internal/sipaddr"
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

// btStopTimeout bounds waiting for the Bluetooth goroutine at shutdown: BlueZ D-Bus calls
// cannot be cancelled, so a stuck one must not hold up the rest of the sequence.
var btStopTimeout = 5 * time.Second

// bluetoothRunner is runBluetooth; tests inject a fake so they never touch real adapters.
type bluetoothRunner func(ctx context.Context, cfg config.Config, coord coordinator, d errorSink, logger *slog.Logger)

// Run starts the daemon and blocks until ctx ends (nil) or a component fails
// (that error). Every goroutine it started has returned when it does, except a
// Bluetooth goroutine stuck in an uncancellable BlueZ call for more than 5 s (logged).
func Run(ctx context.Context, cfg config.Config, password string, logger *slog.Logger) error {
	return run(ctx, cfg, password, logger, runBluetooth)
}

func run(ctx context.Context, cfg config.Config, password string, logger *slog.Logger, bt bluetoothRunner) error {
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
	var st stops
	st.coord, st.bt, st.reg, st.hooks, st.ctl, st.ua = nop, nop, nop, nop, nop, nop
	defer st.run()

	dispatcher, stopHooks := startHooks(cfg.Hooks, logger)
	st.hooks = stopHooks

	nextHop, err := sipaddr.NextHop(cfg)
	if err != nil {
		return err
	}
	ua, err := sip.NewUA(sip.Options{
		Transport: cfg.SIP.Transport,
		NextHop:   nextHop,
		LocalPort: localSIPPort(cfg),
		Username:  cfg.SIP.Username,
		Password:  password,
		UserAgent: "siptooth/" + buildinfo.Version,
		Logger:    logger,
	})
	if err != nil {
		return err
	}
	st.ua = func() { _ = ua.Close() }

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
	st.coord = func() {
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
		st.reg = registrationStop(rep, regCancel, reg.Load().Unregister, logger)
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
	st.ctl = func() { ctlCancel(); <-ctlDone }

	btCtx, btCancel := context.WithCancel(context.Background())
	btDone := make(chan struct{})
	go func() { defer close(btDone); bt(btCtx, cfg, coord, dispatcher, logger) }()
	st.bt = func() {
		btCancel()
		select {
		case <-btDone:
		case <-time.After(btStopTimeout):
			logger.Warn("daemon: bluetooth did not stop in time (a BlueZ call is stuck); continuing shutdown", "timeout", btStopTimeout)
		}
	}

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

// registrationStop is the registration shutdown step. The reporter is silenced first: a
// final Unregister that fails is not a registration outage and must emit no sip_registration.
func registrationStop(rep *registrationReporter, cancel func(), unregister func(context.Context) error, logger *slog.Logger) func() {
	return func() {
		rep.stopping.Store(true)
		cancel()
		uctx, ucancel := context.WithTimeout(context.Background(), unregisterTimeout)
		defer ucancel()
		if err := unregister(uctx); err != nil {
			logger.Warn("sip: unregister", "err", err)
		}
	}
}

// localSIPPort is the SIP port the user agent binds: sip.local_port, which
// defaults to 5060 for UDP (spec 5.5) so the advertised Contact is a known,
// reachable port. TCP and TLS stay ephemeral at 0.
func localSIPPort(cfg config.Config) int {
	if cfg.SIP.LocalPort == 0 && strings.EqualFold(cfg.SIP.Transport, "udp") {
		return 5060
	}
	return cfg.SIP.LocalPort
}

// stops are the component shutdown steps, run in a fixed order: the
// coordinator first (it ends the call in progress, which needs Bluetooth and
// SIP), then Bluetooth, the registration, the hook dispatcher (so the final
// call.ended is delivered) and finally the control socket and the user agent.
type stops struct{ coord, bt, reg, hooks, ctl, ua func() }

func (s *stops) run() {
	s.coord()
	s.bt()
	s.reg()
	s.hooks()
	s.ctl()
	s.ua()
}

// startHooks runs the hook dispatcher. stop lets it deliver the events already
// queued (up to hooksDrainTimeout) before ending it.
func startHooks(hs []config.Hook, logger *slog.Logger) (*hooks.Dispatcher, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	d := hooks.NewDispatcher(hs, logger)
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()
	return d, func() {
		deadline := time.Now().Add(hooksDrainTimeout)
		for d.Outstanding() > 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if n := d.Outstanding(); n > 0 {
			logger.Warn("daemon: hook events undelivered at shutdown", "count", n)
		}
		cancel()
		<-done
	}
}
