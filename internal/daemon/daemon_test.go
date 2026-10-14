package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gosip "github.com/emiago/sipgo/sip"

	"github.com/volodymyrsmirnov/siptooth/internal/bridge"
	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/control"
	"github.com/volodymyrsmirnov/siptooth/internal/hooks"
	"github.com/volodymyrsmirnov/siptooth/internal/sip"
	"github.com/volodymyrsmirnov/siptooth/internal/sip/siptest"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testConfig(t *testing.T, tr *siptest.Trunk) config.Config {
	t.Helper()
	dir, err := os.MkdirTemp("", "st") // short path: unix sockets are limited to ~104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	cfg := config.Default()
	cfg.Bluetooth.PhoneAddress = "AA:BB:CC:DD:EE:FF"
	cfg.Bluetooth.PowerCycleMinutes = 0
	cfg.SIP.Transport = tr.Transport()
	cfg.SIP.Target = tr.URI("200")
	cfg.Control.Socket = filepath.Join(dir, "c.sock")
	return cfg
}

// startDaemon runs the daemon until the test ends and waits for the control
// socket. stop cancels it and returns Run's result (idempotent).
func startDaemon(t *testing.T, cfg config.Config, password string) (c *control.Client, stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, cfg, password, quietLogger()) }()
	var once sync.Once
	var runErr error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case runErr = <-errCh:
			case <-time.After(10 * time.Second):
				t.Error("daemon did not stop")
			}
		})
		return runErr
	}
	t.Cleanup(func() { stop() })
	c = control.NewClient(cfg.Control.Socket)
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if _, err := c.Status(context.Background()); err == nil {
			return c, stop
		}
	}
	t.Fatal("control socket never came up")
	return nil, nil
}

func TestStatusEndpoint(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	cfg := testConfig(t, tr)
	c, stop := startDaemon(t, cfg, "")

	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "idle" || st.Ready || st.PhoneConnected || st.CallID != "" || st.LastOutcome != "" || st.Registration.Enabled {
		t.Fatalf("idle status = %+v", st)
	}
	if err := stop(); err != nil {
		t.Fatalf("clean stop returned %v", err)
	}
	if _, err := os.Stat(cfg.Control.Socket); !os.IsNotExist(err) {
		t.Fatal("control socket must be removed on stop")
	}
}

func TestDaemonRefusesSecondInstance(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	cfg := testConfig(t, tr)
	startDaemon(t, cfg, "")
	err := Run(context.Background(), cfg, "", quietLogger())
	if !errors.Is(err, control.ErrAlreadyRunning) {
		t.Fatalf("second instance: %v", err)
	}
}

func TestRegistrationShownInStatusAndRemovedAtShutdown(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	tr.OnRegister(func(req *gosip.Request, tx gosip.ServerTransaction) {
		res := gosip.NewResponseFromRequest(req, 200, "OK", nil)
		if c := req.Contact(); c != nil {
			cc := gosip.HeaderClone(c).(*gosip.ContactHeader)
			exp := "120"
			if h := req.GetHeader("Expires"); h != nil {
				exp = h.Value()
			}
			cc.Params.Add("expires", exp)
			res.AppendHeader(cc)
		}
		tx.Respond(res)
	})
	cfg := testConfig(t, tr)
	cfg.SIP.Register, cfg.SIP.Username = true, "alice"
	c, stop := startDaemon(t, cfg, "s3cret")

	var st bridge.Status
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		var err error
		if st, err = c.Status(context.Background()); err == nil && st.Registration.Registered {
			break
		}
	}
	r := st.Registration
	if !r.Enabled || !r.Registered || r.Error != "" || r.Expires == nil || !strings.HasPrefix(r.Contact, "sip:alice@127.0.0.1:") {
		t.Fatalf("registration = %+v", r)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	var last siptest.Request
	for _, q := range tr.RequestsFor("REGISTER") {
		last = q
	}
	if last.Header("Expires") != "0" {
		t.Fatalf("last REGISTER Expires = %q, want 0 (unregister at shutdown)", last.Header("Expires"))
	}
}

type recordedErrors struct {
	mu    sync.Mutex
	codes []string
}

func (r *recordedErrors) EmitError(code, detail string) {
	r.mu.Lock()
	r.codes = append(r.codes, code)
	r.mu.Unlock()
}

func (r *recordedErrors) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.codes...)
}

func TestRegistrationErrorEmittedOncePerOutage(t *testing.T) {
	var errs recordedErrors
	var last bridge.RegistrationStatus
	rep := &registrationReporter{user: "alice", transport: "udp", errors: &errs, log: quietLogger(),
		status: func(s bridge.RegistrationStatus) { last = s }}
	flow := netip.MustParseAddrPort("203.0.113.5:5070")
	boom := errors.New("registrar answered 403")

	rep.onState(sip.RegState{})                                     // starting: no outage yet
	rep.onState(sip.RegState{Err: boom})                            // outage begins
	rep.onState(sip.RegState{Err: errors.New("registrar timeout")}) // still the same outage
	rep.onState(sip.RegState{Err: boom})                            // and again
	if got := errs.list(); len(got) != 1 || got[0] != "sip_registration" {
		t.Fatalf("one outage: %v", got)
	}
	if last.Registered || last.Error == "" || !last.Enabled {
		t.Fatalf("status during outage: %+v", last)
	}

	exp := time.Now().Add(time.Minute)
	rep.onState(sip.RegState{Registered: true, Contact: flow, Expires: exp}) // recovered
	if !last.Registered || last.Error != "" || last.Expires == nil || last.Contact != "sip:alice@203.0.113.5:5070;transport=udp" {
		t.Fatalf("status after recovery: %+v", last)
	}
	if len(errs.list()) != 1 {
		t.Fatalf("recovery emitted: %v", errs.list())
	}
	rep.onState(sip.RegState{Err: boom}) // second outage
	rep.onState(sip.RegState{Err: boom})
	if got := errs.list(); len(got) != 2 {
		t.Fatalf("two outages: %v", got)
	}
}

func TestLocalSIPPort(t *testing.T) {
	for _, tc := range []struct {
		transport  string
		port, want int
	}{
		{"udp", 0, 5060}, {"UDP", 0, 5060}, {"udp", 5070, 5070}, {"tcp", 0, 0}, {"tls", 0, 0}, {"tls", 5071, 5071},
	} {
		cfg := config.Default()
		cfg.SIP.Transport, cfg.SIP.LocalPort = tc.transport, tc.port
		if got := localSIPPort(cfg); got != tc.want {
			t.Errorf("%s/%d: got %d, want %d", tc.transport, tc.port, got, tc.want)
		}
	}
}

func TestRegistrationErrorSuppressedWhileStopping(t *testing.T) {
	var errs recordedErrors
	rep := &registrationReporter{user: "alice", transport: "udp", errors: &errs, log: quietLogger(),
		status: func(bridge.RegistrationStatus) {}}
	rep.stopping.Store(true) // shutdown began: the unregister failing is not an outage
	rep.onState(sip.RegState{Err: errors.New("unregister timed out")})
	if got := errs.list(); len(got) != 0 {
		t.Fatalf("emitted during shutdown: %v", got)
	}
}

// The coordinator ends the call in progress at shutdown and emits call.ended;
// the stop sequence must deliver it before the dispatcher ends.
func TestShutdownDeliversCallEnded(t *testing.T) {
	out := filepath.Join(t.TempDir(), "events")
	d, stopHooks := startHooks([]config.Hook{{
		ID: "h", Events: []string{"call.ended"}, TimeoutSeconds: 5,
		Command: "cat >> " + out,
	}}, quietLogger())
	nop := func() {}
	st := stops{coord: func() { d.Emit("call.ended", hooks.CallInfo{ID: "c1"}) },
		bt: nop, reg: nop, hooks: stopHooks, ctl: nop, ua: nop}
	st.run()
	b, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(b), `"call.ended"`) {
		t.Fatalf("call.ended not delivered: %q %v", b, err)
	}
}
