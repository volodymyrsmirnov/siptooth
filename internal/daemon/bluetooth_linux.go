//go:build linux

package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/bluez"
	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/hfp"
	"github.com/volodymyrsmirnov/siptooth/internal/sco"
)

const (
	bluezRetryInterval   = 10 * time.Second // bluetoothd / adapter not reachable at start
	connectInterval      = 30 * time.Second // ConnectProfile while not connected (spec 3.1)
	slcRetryDelay        = 5 * time.Second  // ConnectProfile after an SLC failure (spec 3.3)
	connectTimeout       = 15 * time.Second
	slcTimeout           = 10 * time.Second
	bluezRestartSettle   = 2 * time.Second
	adapterAlias         = "siptooth"
	profileRegisterLimit = 10 * time.Second
	routingRetryInterval = time.Minute // routing that failed or needs CAP_NET_RAW is re-checked this often
)

// btHandler is the single owner of the live HFP client and SCO listener.
type btHandler struct {
	coord       coordinator
	adapterAddr func() string
	log         *slog.Logger
	slcFailed   func() // schedules a ConnectProfile retry

	mu       sync.Mutex
	client   *hfp.Client
	listener sco.Listener
	discGen  int // bumped by every Disconnected; an SLC that raced one is dropped
}

// NewConnection runs the SLC on the RFCOMM fd and hands the client to the coordinator.
func (h *btHandler) NewConnection(device string, fd *os.File) error {
	h.mu.Lock()
	gen := h.discGen
	h.mu.Unlock()
	c := hfp.New(fd, h.log)
	ctx, cancel := context.WithTimeout(context.Background(), slcTimeout)
	defer cancel()
	if err := c.Start(ctx); err != nil { // Start closes the transport on failure
		h.log.Warn("bluetooth: SLC failed, retrying in 5 s", "device", device, "err", err)
		h.slcFailed()
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.discGen != gen {
		// RequestDisconnection/Release arrived while the SLC was being set up: BlueZ wants
		// this connection gone, so do not attach it.
		h.log.Info("bluetooth: disconnect requested during SLC setup; dropping the connection", "device", device)
		c.Close()
		return errors.New("bluetooth: disconnected during SLC setup")
	}
	// Release the previous connection before binding a new listener (same adapter address:
	// EADDRINUSE otherwise). Detach makes a call on the old link interrupted.
	h.releaseLocked()
	lis, err := sco.Listen(h.adapterAddr())
	if err != nil {
		h.log.Error("bluetooth: SCO listen failed", "err", err)
		c.Close()
		h.slcFailed()
		return err
	}
	h.client, h.listener = c, lis
	h.coord.Attach(c, lis, h.adapterAddr()) // under mu: Attach/Detach reach the coordinator in order
	h.log.Info("bluetooth: HFP connected", "device", device)
	return nil
}

// Disconnected tears down the live connection. device may be empty (BlueZ loss, shutdown).
func (h *btHandler) Disconnected(device string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.discGen++
	if h.client != nil {
		h.log.Info("bluetooth: HFP disconnected", "device", device)
	}
	h.releaseLocked()
}

func (h *btHandler) releaseLocked() {
	if h.client == nil && h.listener == nil {
		return
	}
	if h.client != nil {
		h.client.Close()
		h.client = nil
	}
	if h.listener != nil {
		h.listener.Close()
		h.listener = nil
	}
	// Explicit, because the hfp Disconnected event may be dropped once the client is closed.
	h.coord.Detach()
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// runBluetooth runs Bluetooth sessions until ctx ends. A session ends early only when the
// system bus connection is lost; a new one (new connection, profile, routing) starts 10 s
// later (production review 2026-10-05: the daemon kept a dead connection forever).
func runBluetooth(ctx context.Context, cfg config.Config, coord coordinator, d errorSink, logger *slog.Logger) {
	for runBluetoothSession(ctx, cfg, coord, d, logger) {
		if !sleepCtx(ctx, bluezRetryInterval) {
			return
		}
	}
}

// runBluetoothSession runs one system bus connection; it reports whether the bus was lost
// (false: ctx ended).
func runBluetoothSession(ctx context.Context, cfg config.Config, coord coordinator, d errorSink, logger *slog.Logger) bool {
	var m *bluez.Manager
	for reported := false; ; {
		var err error
		m, err = bluez.Connect(ctx, cfg.Bluetooth.Adapter, cfg.Bluetooth.PhoneAddress, logger)
		if err == nil {
			break
		}
		logger.Error("bluetooth: not available, retrying in 10 s", "err", err)
		if !reported { // one hook per outage, not one every 10 s
			d.EmitError("bluetooth_unavailable", err.Error())
			reported = true
		}
		if !sleepCtx(ctx, bluezRetryInterval) {
			return false
		}
	}
	defer m.Close()
	ctx, cancel := context.WithCancel(ctx) // ends the session's workers when the bus is lost
	defer cancel()

	retry := make(chan struct{}, 1)
	scheduleRetry := func() {
		select {
		case retry <- struct{}{}:
		default:
		}
	}
	h := &btHandler{coord: coord, adapterAddr: m.AdapterAddress, log: logger, slcFailed: scheduleRetry}

	// routing verifies the Broadcom SCO routing and feeds readiness: no verified audio path,
	// no answering (spec sections 3.5 and 6).
	routingReported := false          // one sco_routing hook per outage; touched only on this goroutine
	var routingRetry <-chan time.Time // armed while routing cannot be verified yet
	routing := func() {
		routingRetry = nil
		on, err := m.Powered()
		if err != nil || !on {
			logger.Warn("bluetooth: adapter not powered, audio routing unverified", "err", err)
			coord.SetAudioRouting(false)
			if err == nil { // the power-on in register may have failed: try again
				if perr := m.SetPowered(true); perr != nil {
					logger.Warn("bluetooth: power on", "err", perr)
				}
			}
			// AdapterUp re-runs routing once the adapter powers on; the retry covers a failed
			// read or power-on that no adapter event would ever follow (production review 2026-10-05).
			routingRetry = time.After(bluezRetryInterval)
			return
		}
		manu, merr := m.Manufacturer()
		if merr != nil {
			manu, merr = m.RefreshManufacturer()
		}
		switch bluez.PlanRouting(manu, merr) {
		case bluez.RoutingUnknown:
			// Never grant the non-Broadcom exemption without a successful read: a Broadcom
			// controller with an unreadable property would otherwise answer without audio.
			logger.Warn("bluetooth: adapter manufacturer unknown; audio routing unverified, retrying in 10 s", "err", merr)
			if !routingReported {
				d.EmitError("routing", "adapter manufacturer unknown: "+merr.Error())
				routingReported = true
			}
			coord.SetAudioRouting(false)
			routingRetry = time.After(bluezRetryInterval)
			return
		case bluez.RoutingNotNeeded:
			logger.Info("bluetooth: non-Broadcom adapter, SCO routing command skipped", "manufacturer", manu)
			routingReported = false
			coord.SetAudioRouting(true)
			return
		}
		before, after, err := bluez.EnsureSCORouting(m.AdapterIndex(), logger)
		switch bluez.JudgeRouting(after, err) {
		case bluez.RoutingAssumed:
			// The service runs without CAP_NET_RAW, so the routing usually cannot be read.
			// It is assumed (Raspberry Pi OS's bthelper routes SCO over HCI at boot);
			// `siptooth doctor` reports it as unverified.
			logger.Warn("bluetooth: cannot read SCO routing without CAP_NET_RAW; assuming it is routed over HCI", "err", err)
		case bluez.RoutingNeedsCapability:
			logger.Error("bluetooth: SCO routing is not HCI and changing it needs CAP_NET_RAW once (run `siptooth doctor`; SCO routing needs CAP_NET_RAW once, see docs/operations.md); calls will not be answered", "before", before, "err", err)
			if !routingReported {
				d.EmitError("routing", err.Error())
				routingReported = true
			}
			coord.SetAudioRouting(false)
			routingRetry = time.After(routingRetryInterval) // picks up a fix (CAP_NET_RAW granted once) without a restart
			return
		case bluez.RoutingFailed:
			if err != nil {
				logger.Warn("bluetooth: SCO routing failed; calls will not be answered", "err", err)
				if !routingReported {
					d.EmitError("routing", err.Error())
					routingReported = true
				}
			} else {
				logger.Error("bluetooth: SCO routing still not HCI", "before", before, "after", after)
			}
			coord.SetAudioRouting(false)
			routingRetry = time.After(routingRetryInterval)
			return
		}
		routingReported = false
		coord.SetAudioRouting(true)
	}
	var registerRetry <-chan time.Time // armed while the HF profile is not registered
	registerReported := false          // one profile_registration hook per outage
	register := func() {
		registerRetry = nil
		if err := m.SetPowered(true); err != nil {
			logger.Warn("bluetooth: power on", "err", err)
		}
		if err := m.SetAlias(adapterAlias); err != nil {
			logger.Warn("bluetooth: set alias", "err", err)
		}
		rctx, cancel := context.WithTimeout(ctx, profileRegisterLimit)
		defer cancel()
		if err := m.RegisterProfile(rctx, h); err != nil {
			// Without the profile the phone cannot connect, so keep trying instead of waiting
			// for a restart (production review 2026-10-05).
			logger.Error("bluetooth: profile registration failed, retrying in 10 s", "err", err)
			if !registerReported {
				d.EmitError("profile_registration", err.Error())
				registerReported = true
			}
			registerRetry = time.After(bluezRetryInterval)
			return
		}
		registerReported = false
	}

	// Changes are handled on this goroutine; the Watch callback only forwards them.
	changes := make(chan bluez.Change, 32)
	watchEnded := make(chan struct{}) // Watch returns before ctx ends only when the bus is gone
	var bg sync.WaitGroup
	bg.Add(1)
	go func() {
		defer bg.Done()
		defer close(watchEnded)
		m.Watch(ctx, func(ch bluez.Change) {
			select {
			case changes <- ch:
			case <-ctx.Done():
			}
		})
	}()
	defer func() {
		h.Disconnected("") // shutdown: release the phone link before the bus connection closes
		cancel()
		bg.Wait()
	}()

	// The periodic power cycle (iteration 7): while cycling, the adapter is off on purpose, so
	// adapter events must not power it on again before powerOn fires.
	var cycleDue <-chan time.Time // nil when power_cycle_minutes is 0
	every := powerCycleInterval(cfg.Bluetooth)
	if every > 0 {
		cycleDue = time.After(every)
	}
	var powerOn <-chan time.Time // armed while the cycle holds the adapter off
	cycling := false
	cycle := func() {
		if !powerCycleAllowed(coord.Status()) {
			cycleDue = time.After(powerCycleBusyRetry)
			return
		}
		logger.Info("bluetooth: scheduled adapter power cycle")
		coord.SetAudioRouting(false) // no answering until routing() verifies the adapter again
		if err := m.SetPowered(false); err != nil {
			logger.Warn("bluetooth: power cycle: power off", "err", err)
			routing()
			cycleDue = time.After(every)
			return
		}
		cycling = true
		powerOn = time.After(powerCycleOffTime)
	}

	register() // powers the adapter
	routing()
	// The connect loop starts only now: its first ConnectProfile needs the adapter powered
	// and the HF profile registered, or the phone would connect only on the 30 s tick.
	bg.Add(1)
	go func() { defer bg.Done(); connectLoop(ctx, m, coord, retry, logger) }()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-watchEnded:
			if ctx.Err() != nil {
				return false
			}
			logger.Error("bluetooth: system bus connection lost, reconnecting in 10 s")
			d.EmitError("bluetooth_unavailable", "system bus connection lost")
			coord.SetAudioRouting(false)
			return true
		case <-routingRetry:
			if !cycling { // powerOn runs routing itself
				routing()
			}
		case <-cycleDue:
			cycle()
		case <-powerOn:
			powerOn, cycling = nil, false
			routing()       // powers the adapter on; AdapterUp then verifies it again
			scheduleRetry() // reconnect the phone in 5 s rather than on the 30 s tick
			cycleDue = time.After(every)
		case <-registerRetry:
			register()
			if registerRetry == nil {
				scheduleRetry() // registered: connect the phone now rather than on the 30 s tick
			}
		case ch := <-changes:
			switch ch.Kind {
			case bluez.AdapterUp: // also fires for a merely present adapter: routing re-checks Powered
				if cycling {
					break // powerOn runs routing once the adapter has been off long enough
				}
				logger.Info("bluetooth: adapter up")
				routing()
			case bluez.AdapterDown:
				if cycling {
					logger.Info("bluetooth: adapter off for the power cycle")
					break
				}
				logger.Warn("bluetooth: adapter down")
				routing() // revokes readiness, powers the adapter on again and keeps retrying
			case bluez.BluezLost:
				logger.Warn("bluetooth: bluetoothd left the bus")
				coord.SetAudioRouting(false)
				h.Disconnected("") // the current call becomes interrupted
			case bluez.BluezRestarted:
				logger.Warn("bluetooth: bluetoothd restarted, re-registering")
				h.Disconnected("")
				if !sleepCtx(ctx, bluezRestartSettle) {
					return false
				}
				register()
				routing()
				scheduleRetry()
			case bluez.PhoneConnected:
				logger.Info("bluetooth: phone connected")
			case bluez.PhoneDisconnected:
				logger.Info("bluetooth: phone disconnected")
			}
		}
	}
}

// connectLoop calls ConnectProfile at start, every 30 s, and 5 s after a requested retry,
// whenever no SLC is up (SLC-level readiness, not merely Device1.Connected).
func connectLoop(ctx context.Context, m *bluez.Manager, coord coordinator, retry <-chan struct{}, logger *slog.Logger) {
	attempt := func() {
		if coord.Status().PhoneConnected {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, connectTimeout)
		defer cancel()
		if err := m.ConnectProfile(cctx); err != nil {
			logger.Debug("bluetooth: connect attempt", "err", err)
		}
	}
	attempt()
	t := time.NewTicker(connectInterval)
	defer t.Stop()
	var retryTimer *time.Timer
	var retryC <-chan time.Time
	defer func() {
		if retryTimer != nil {
			retryTimer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			attempt()
		case <-retry:
			if retryC == nil {
				retryTimer = time.NewTimer(slcRetryDelay)
				retryC = retryTimer.C
			}
		case <-retryC:
			retryC = nil
			attempt()
		}
	}
}
