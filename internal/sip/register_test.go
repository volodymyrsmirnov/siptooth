package sip

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gosip "github.com/emiago/sipgo/sip"

	"github.com/volodymyrsmirnov/siptooth/internal/sip/siptest"
)

const regNonce = "abc123nonce"

// fakeRegistrar is a digest-protected registrar behind a trunk.
type fakeRegistrar struct {
	tr    *siptest.Trunk
	grant int // seconds granted per REGISTER; 0 grants what was asked

	failFirst atomic.Int32 // answer this many REGISTERs 500
	flowIP    atomic.Value // string: overrides Via received
	flowPort  atomic.Int32 // overrides Via rport when > 0

	mu       sync.Mutex
	okTimes  []time.Time // authenticated REGISTERs
	okReqs   []siptest.Request
	posted   int
	unauthed int
}

func newFakeRegistrar(tr *siptest.Trunk, grant int) *fakeRegistrar {
	r := &fakeRegistrar{tr: tr, grant: grant}
	tr.OnRegister(r.handle)
	return r
}

func (r *fakeRegistrar) handle(req *gosip.Request, tx gosip.ServerTransaction) {
	if r.failFirst.Load() > 0 {
		r.failFirst.Add(-1)
		tx.Respond(gosip.NewResponseFromRequest(req, 500, "Server Internal Error", nil))
		return
	}
	if !siptest.CheckDigest(req, testUser, testPass, regNonce) {
		r.mu.Lock()
		r.unauthed++
		r.mu.Unlock()
		siptest.Challenge(req, tx, false, testRealm, regNonce, false)
		return
	}
	asked := 0
	if h := req.GetHeader("Expires"); h != nil {
		asked, _ = strconv.Atoi(h.Value())
	}
	res := gosip.NewResponseFromRequest(req, 200, "OK", nil)
	granted := asked
	if r.grant > 0 && asked > r.grant {
		granted = r.grant
	}
	if c := req.Contact(); c != nil {
		cc := gosip.HeaderClone(c).(*gosip.ContactHeader)
		cc.Params.Add("expires", strconv.Itoa(granted))
		res.AppendHeader(cc)
	}
	if v, _ := r.flowIP.Load().(string); v != "" {
		res.Via().Params.Add("received", v)
	}
	if p := r.flowPort.Load(); p > 0 {
		res.Via().Params.Add("rport", strconv.Itoa(int(p)))
	}
	r.mu.Lock()
	r.okTimes = append(r.okTimes, time.Now())
	r.mu.Unlock()
	tx.Respond(res)
}

func (r *fakeRegistrar) authenticated() []siptest.Request {
	var out []siptest.Request
	for _, q := range r.tr.RequestsFor("REGISTER") {
		if q.Header("Authorization") != "" {
			out = append(out, q)
		}
	}
	return out
}

func (r *fakeRegistrar) times() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.okTimes...)
}

type stateLog struct {
	mu     sync.Mutex
	states []RegState
}

func (l *stateLog) add(s RegState) {
	l.mu.Lock()
	l.states = append(l.states, s)
	l.mu.Unlock()
}

func (l *stateLog) all() []RegState {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]RegState(nil), l.states...)
}

func (l *stateLog) last() RegState {
	s := l.all()
	if len(s) == 0 {
		return RegState{}
	}
	return s[len(s)-1]
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

func regUA(t *testing.T, tr *siptest.Trunk, mod func(*UA)) *UA {
	t.Helper()
	u := newTestUA(t, tr, func(o *Options) {
		o.Username, o.Password = testUser, testPass
		if tr.Transport() == "udp" {
			o.LocalPort = freeUDPPort(t)
		}
	})
	u.regRetry = 100 * time.Millisecond
	if mod != nil {
		mod(u)
	}
	return u
}

func startReg(t *testing.T, u *UA, tr *siptest.Trunk, expires time.Duration) (*Registration, *stateLog, context.CancelFunc) {
	t.Helper()
	var log stateLog
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := u.Register(ctx, "", tr.HostPort(), expires, log.add)
	return r, &log, cancel
}

func waitState(t *testing.T, l *stateLog, what string, cond func(RegState) bool) RegState {
	t.Helper()
	var got RegState
	eventually(t, what, func() bool {
		for _, s := range l.all() {
			if cond(s) {
				got = s
				return true
			}
		}
		return false
	})
	return got
}

func registered(s RegState) bool { return s.Registered }

func contactAddr(t *testing.T, q siptest.Request) netip.AddrPort {
	t.Helper()
	c := q.Req.Contact()
	if c == nil {
		t.Fatalf("REGISTER without Contact: %v", q.Headers)
	}
	ip, err := netip.ParseAddr(c.Address.Host)
	if err != nil {
		t.Fatal(err)
	}
	return netip.AddrPortFrom(ip, uint16(c.Address.Port))
}

func TestRegisterChallengeAndRefresh(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	reg := newFakeRegistrar(tr, 4)
	u := regUA(t, tr, nil)
	r, log, _ := startReg(t, u, tr, 120*time.Second)

	st := waitState(t, log, "registered", registered)
	if st.Err != nil || !st.Contact.IsValid() {
		t.Fatalf("state = %+v", st)
	}
	if d := time.Until(st.Expires); d < 2*time.Second || d > 5*time.Second {
		t.Errorf("Expires in %v, want about the granted 4 s", d)
	}
	eventually(t, "a refresh", func() bool { return len(reg.times()) >= 2 })
	times := reg.times()
	if gap := times[1].Sub(times[0]); gap < 1500*time.Millisecond || gap > 3500*time.Millisecond {
		t.Errorf("refresh after %v, want about 2 s (half of the granted 4 s)", gap)
	}
	first := tr.RequestsFor("REGISTER")[0]
	if first.Header("Expires") != "120" {
		t.Errorf("first Expires = %q", first.Header("Expires"))
	}
	if first.Header("Authorization") != "" {
		t.Error("first REGISTER carried credentials")
	}
	if !strings.HasPrefix(first.RequestURI, "sip:") || strings.Contains(first.RequestURI, "@") {
		t.Errorf("Request-URI = %q, want the registrar without user", first.RequestURI)
	}
	if to := first.Header("To"); !strings.Contains(to, "sip:"+testUser+"@"+tr.HostPort()) {
		t.Errorf("To = %q, want the default AOR", to)
	}
	if got := r.State(); !got.Registered {
		t.Errorf("State() = %+v", got)
	}
	// Same dialog identifiers, increasing CSeq.
	rs := tr.RequestsFor("REGISTER")
	if rs[0].Header("Call-ID") != rs[len(rs)-1].Header("Call-ID") {
		t.Error("Call-ID changed between REGISTERs")
	}
	closeBounded(t, u)
}

func TestRegisterContactFromVia(t *testing.T) {
	for _, tp := range allTransports {
		t.Run(tp, func(t *testing.T) {
			tr := siptest.NewTrunk(t, tp)
			reg := newFakeRegistrar(tr, 0)
			u := regUA(t, tr, nil)
			_, log, _ := startReg(t, u, tr, 60*time.Second)

			st := waitState(t, log, "registered", registered)
			reqs := reg.authenticated()
			if len(reqs) == 0 {
				t.Fatal("no authenticated REGISTER")
			}
			src, err := netip.ParseAddrPort(reqs[0].Source)
			if err != nil {
				t.Fatal(err)
			}
			if st.Contact != src {
				t.Errorf("Contact = %v, want the flow the registrar saw (%v)", st.Contact, src)
			}
			// The last REGISTER already carries the discovered flow.
			eventually(t, "REGISTER with the flow Contact", func() bool {
				rs := reg.authenticated()
				return contactAddr(t, rs[len(rs)-1]) == src
			})
			closeBounded(t, u)
		})
	}
}

func TestReregisterOnMappingChange(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	reg := newFakeRegistrar(tr, 2)
	u := regUA(t, tr, nil)
	reg.flowIP.Store("203.0.113.5")
	reg.flowPort.Store(40000)
	_, log, _ := startReg(t, u, tr, 60*time.Second)

	waitState(t, log, "first mapping", func(s RegState) bool {
		return s.Registered && s.Contact == netip.MustParseAddrPort("203.0.113.5:40000")
	})
	eventually(t, "REGISTER carrying the first mapping", func() bool {
		rs := reg.authenticated()
		return len(rs) > 0 && contactAddr(t, rs[len(rs)-1]) == netip.MustParseAddrPort("203.0.113.5:40000")
	})
	before := len(reg.authenticated())

	reg.flowPort.Store(40001) // the NAT rebinds
	waitState(t, log, "second mapping", func(s RegState) bool {
		return s.Registered && s.Contact == netip.MustParseAddrPort("203.0.113.5:40001")
	})
	eventually(t, "REGISTER carrying the new mapping", func() bool {
		rs := reg.authenticated()
		return len(rs) > before && contactAddr(t, rs[len(rs)-1]) == netip.MustParseAddrPort("203.0.113.5:40001")
	})
	closeBounded(t, u)
}

func TestRegisterRetryAfterFailure(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	reg := newFakeRegistrar(tr, 0)
	reg.failFirst.Store(2)
	u := regUA(t, tr, nil)
	_, log, _ := startReg(t, u, tr, 60*time.Second)

	failed := waitState(t, log, "failure state", func(s RegState) bool { return !s.Registered && s.Err != nil })
	if !strings.Contains(failed.Err.Error(), "500") {
		t.Errorf("Err = %v", failed.Err)
	}
	waitState(t, log, "registered after retries", registered)
	n500 := 0
	for _, s := range log.all() {
		if !s.Registered && s.Err != nil {
			n500++
		}
	}
	if n500 < 1 {
		t.Errorf("no failure state recorded: %+v", log.all())
	}
	closeBounded(t, u)
}

func TestUnregisterExpires0(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	reg := newFakeRegistrar(tr, 0)
	u := regUA(t, tr, nil)
	r, log, _ := startReg(t, u, tr, 60*time.Second)
	st := waitState(t, log, "registered", registered)
	eventually(t, "settled on the flow Contact", func() bool {
		rs := reg.authenticated()
		return len(rs) > 0 && contactAddr(t, rs[len(rs)-1]) == r.State().Contact
	})
	st = r.State()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Unregister(ctx); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	rs := reg.authenticated()
	last := rs[len(rs)-1]
	if last.Header("Expires") != "0" {
		t.Errorf("Expires = %q, want 0", last.Header("Expires"))
	}
	if got := last.Header("Contact"); strings.Contains(got, "*") || contactAddr(t, last) != st.Contact {
		t.Errorf("Contact = %q, want only our own %v", got, st.Contact)
	}
	if s := r.State(); s.Registered {
		t.Errorf("still registered after Unregister: %+v", s)
	}
	if s := log.last(); s.Registered {
		t.Errorf("last callback state = %+v", s)
	}
	n := len(tr.RequestsFor("REGISTER"))
	time.Sleep(300 * time.Millisecond)
	if m := len(tr.RequestsFor("REGISTER")); m != n {
		t.Errorf("registration loop still running after Unregister (%d -> %d)", n, m)
	}
	if err := r.Unregister(ctx); err != nil {
		t.Errorf("second Unregister: %v", err)
	}
	closeBounded(t, u)
}

func TestRegisteredInvite603(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	newFakeRegistrar(tr, 0)
	u := regUA(t, tr, nil)
	r, log, _ := startReg(t, u, tr, 60*time.Second)
	waitState(t, log, "registered", registered)
	c := r.State().Contact
	res, err := tr.SendRequest(ctx5(t), "INVITE", "sip:"+testUser+"@"+c.String(), reofferBody(30000, ""))
	if err != nil || res.StatusCode != 603 {
		t.Fatalf("INVITE to the registered Contact = %v, %v, want 603", res, err)
	}
	closeBounded(t, u)
}

func TestRegisterThenInviteSameConnection(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp")
	newFakeRegistrar(tr, 0)
	tr.OnInvite(answer200(tr))
	u := regUA(t, tr, nil)
	_, log, _ := startReg(t, u, tr, 60*time.Second)
	waitState(t, log, "registered", registered)

	r := result(t, start(t, u, dialRequest(tr, nil)), 5*time.Second)
	if r.Call == nil {
		t.Fatalf("result = %+v", r)
	}
	inv, ok := tr.WaitRequest("INVITE", 1, 5*time.Second)
	if !ok {
		t.Fatal("no INVITE")
	}
	reg := tr.RequestsFor("REGISTER")
	if inv.Source != reg[len(reg)-1].Source {
		t.Errorf("INVITE came from %s, REGISTER from %s: not the same connection", inv.Source, reg[len(reg)-1].Source)
	}
	closeBounded(t, u)
}

func TestIdleKeepaliveBetweenCalls(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp")
	newFakeRegistrar(tr, 0)
	tr.OnInvite(answer200(tr))
	u := regUA(t, tr, func(u *UA) { u.idleKeepalive = 80 * time.Millisecond })
	_, log, _ := startReg(t, u, tr, 60*time.Second)
	waitState(t, log, "registered", registered)
	base := tr.Keepalives()
	eventually(t, "idle keepalives", func() bool { return tr.Keepalives() >= base+3 })

	// During a TCP call the call's own keepalive owns the connection.
	r := result(t, start(t, u, dialRequest(tr, nil)), 5*time.Second)
	if r.Call == nil {
		t.Fatalf("result = %+v", r)
	}
	time.Sleep(100 * time.Millisecond)
	during := tr.Keepalives()
	time.Sleep(500 * time.Millisecond)
	if n := tr.Keepalives() - during; n > 1 {
		t.Errorf("%d registration keepalives during a call", n)
	}
	closeBounded(t, u)
}
