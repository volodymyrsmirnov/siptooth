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

func TestNextHopAddress(t *testing.T) {
	cases := []struct {
		name      string
		mod       func(*config.Config)
		want      string
		wantError bool
	}{
		{"tls default port", func(c *config.Config) { c.SIP.Transport, c.SIP.Target = "tls", "sip:{caller}@pbx.example.com" }, "pbx.example.com:5061", false},
		{"udp default port", func(c *config.Config) { c.SIP.Transport, c.SIP.Target = "udp", "sip:agent@pbx.example.com" }, "pbx.example.com:5060", false},
		{"target port", func(c *config.Config) { c.SIP.Target = "sip:agent@10.0.0.5:5080;transport=tcp" }, "10.0.0.5:5080", false},
		{"proxy wins", func(c *config.Config) { c.SIP.Proxy = "proxy.example.com:5070" }, "proxy.example.com:5070", false},
		{"ipv6", func(c *config.Config) { c.SIP.Target = "sip:agent@[2001:db8::1]:5060" }, "[2001:db8::1]:5060", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.SIP.Target = "sip:x@example.com"
			tc.mod(&cfg)
			got, err := nextHopAddress(cfg)
			if (err != nil) != tc.wantError || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
