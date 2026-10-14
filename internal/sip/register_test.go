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
	// failRemoval answers 500 to any REGISTER that expires a Contact.
	failRemoval atomic.Bool

	mu       sync.Mutex
	okTimes  []time.Time // authenticated REGISTERs
	okReqs   []siptest.Request
	posted   int
	unauthed int
	bound    map[string]*gosip.ContactHeader
	maxBound int
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
	if r.failRemoval.Load() {
		for _, h := range req.GetHeaders("Contact") {
			if c, ok := h.(*gosip.ContactHeader); ok {
				if v, has := c.Params.Get("expires"); has && v == "0" {
					tx.Respond(gosip.NewResponseFromRequest(req, 500, "Server Internal Error", nil))
					return
				}
			}
		}
	}
	res := gosip.NewResponseFromRequest(req, 200, "OK", nil)

	r.mu.Lock()
	if r.bound == nil {
		r.bound = map[string]*gosip.ContactHeader{}
	}
	for _, h := range req.GetHeaders("Contact") {
		c, ok := h.(*gosip.ContactHeader)
		if !ok {
			continue
		}
		key := c.Address.User + "@" + net.JoinHostPort(c.Address.Host, strconv.Itoa(c.Address.Port))
		granted := asked
		if v, has := c.Params.Get("expires"); has {
			granted, _ = strconv.Atoi(v)
		}
		if granted == 0 {
			delete(r.bound, key)
			continue
		}
		if r.grant > 0 && granted > r.grant {
			granted = r.grant
		}
		cc := gosip.HeaderClone(c).(*gosip.ContactHeader)
		cc.Params.Add("expires", strconv.Itoa(granted))
		r.bound[key] = cc
	}
	for _, cc := range r.bound {
		res.AppendHeader(gosip.HeaderClone(cc))
	}
	if n := len(r.bound); n > r.maxBound {
		r.maxBound = n
	}
	r.okTimes = append(r.okTimes, time.Now())
	r.mu.Unlock()

	if v, _ := r.flowIP.Load().(string); v != "" {
		res.Via().Params.Add("received", v)
	}
	if p := r.flowPort.Load(); p > 0 {
		res.Via().Params.Add("rport", strconv.Itoa(int(p)))
	}
	tx.Respond(res)
}

// bindings returns the current bindings as user@host:port.
func (r *fakeRegistrar) bindings() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for k := range r.bound {
		out = append(out, k)
	}
	return out
}

func (r *fakeRegistrar) hasBinding(c netip.AddrPort) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.bound[testUser+"@"+c.String()]
	return ok
}

// maxBindings is the most bindings the registrar ever held after a request.
func (r *fakeRegistrar) maxBindings() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxBound
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
	u.regMinRefresh = 50 * time.Millisecond
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
	hs := q.Req.GetHeaders("Contact")
	if len(hs) == 0 {
		t.Fatalf("REGISTER without Contact: %v", q.Headers)
	}
	c := hs[0].(*gosip.ContactHeader) // the Contact being registered comes first
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

			// Over TCP the first REGISTER carries a guess; the state names the
			// flow only once it is registered.
			st := waitState(t, log, "registered on the flow", func(s RegState) bool {
				return s.Registered && (tp == "udp" || s.Contact.Port() != defaultSIPPort)
			})
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

func TestReregisterReplacesContact(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	reg := newFakeRegistrar(tr, 2)
	u := regUA(t, tr, func(u *UA) { u.regMinRefresh = 100 * time.Millisecond })
	reg.flowIP.Store("203.0.113.5")
	reg.flowPort.Store(40000)
	_, log, _ := startReg(t, u, tr, 60*time.Second)
	waitState(t, log, "first mapping", func(s RegState) bool {
		return s.Registered && s.Contact == netip.MustParseAddrPort("203.0.113.5:40000") && reg.hasBinding(s.Contact)
	})
	reg.flowPort.Store(40001)
	waitState(t, log, "second mapping", func(s RegState) bool {
		return s.Registered && s.Contact == netip.MustParseAddrPort("203.0.113.5:40001") && reg.hasBinding(s.Contact)
	})
	want := testUser + "@203.0.113.5:40001"
	eventually(t, "only the current binding left", func() bool {
		b := reg.bindings()
		return len(b) == 1 && b[0] == want
	})
	if m := reg.maxBindings(); m > 1 {
		t.Errorf("registrar held %d bindings at once; a Contact change must expire the old one in the same REGISTER", m)
	}
	closeBounded(t, u)
}

func TestFirstGuessIsExpiredOnceFlowKnown(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp") // ephemeral client port: the guess is wrong
	reg := newFakeRegistrar(tr, 0)
	u := regUA(t, tr, nil)
	_, log, _ := startReg(t, u, tr, 60*time.Second)
	st := waitState(t, log, "registered on the flow", func(s RegState) bool { return s.Registered && s.Contact.Port() != defaultSIPPort })
	eventually(t, "single binding on the flow", func() bool {
		b := reg.bindings()
		return len(b) == 1 && b[0] == testUser+"@"+st.Contact.String()
	})
	if m := reg.maxBindings(); m > 1 {
		t.Errorf("registrar held %d bindings at once", m)
	}
	closeBounded(t, u)
}

func TestUnregisterRemovesEveryContact(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp")
	reg := newFakeRegistrar(tr, 0)
	reg.failRemoval.Store(true) // the REGISTER that would expire the guess keeps failing
	u := regUA(t, tr, nil)
	r, log, _ := startReg(t, u, tr, 60*time.Second)
	waitState(t, log, "registered on the guess", registered)
	waitState(t, log, "a failed follow-up", func(s RegState) bool { return !s.Registered && s.Err != nil })
	if len(reg.bindings()) != 1 {
		t.Fatalf("bindings = %v, want only the guess", reg.bindings())
	}
	reg.failRemoval.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Unregister(ctx); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if b := reg.bindings(); len(b) != 0 {
		t.Errorf("bindings left after Unregister: %v", b)
	}
	closeBounded(t, u)
}

func TestRegisterPublishesOnlyRegisteredContact(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp")
	reg := newFakeRegistrar(tr, 0)
	u := regUA(t, tr, nil)
	var bad atomic.Value
	var log stateLog
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	u.Register(ctx, "", tr.HostPort(), 60*time.Second, func(s RegState) {
		log.add(s)
		if s.Registered && !reg.hasBinding(s.Contact) {
			bad.Store(s.Contact.String())
		}
	})
	waitState(t, &log, "registered", func(s RegState) bool { return s.Registered && s.Contact.Port() != defaultSIPPort })
	if v := bad.Load(); v != nil {
		t.Errorf("Registered state published Contact %v before it was registered", v)
	}
	closeBounded(t, u)
}

func TestRefreshIntervalIsFloored(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	reg := newFakeRegistrar(tr, 4) // half is 2 s
	u := regUA(t, tr, func(u *UA) { u.regMinRefresh = 3 * time.Second })
	_, log, _ := startReg(t, u, tr, 60*time.Second)
	waitState(t, log, "registered", registered)
	eventually(t, "a refresh", func() bool { return len(reg.times()) >= 2 })
	ts := reg.times()
	if gap := ts[1].Sub(ts[0]); gap < 2600*time.Millisecond {
		t.Errorf("refresh after %v, want at least the 3 s floor", gap)
	}
	closeBounded(t, u)
}

func TestRegisterTwiceIsAnError(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	newFakeRegistrar(tr, 0)
	u := regUA(t, tr, nil)
	first, log, _ := startReg(t, u, tr, 60*time.Second)
	waitState(t, log, "registered", registered)
	var second stateLog
	r2 := u.Register(context.Background(), "", tr.HostPort(), 60*time.Second, second.add)
	if st := r2.State(); st.Registered || st.Err == nil {
		t.Errorf("second Register state = %+v, want an error", st)
	}
	if !first.State().Registered {
		t.Error("the first registration was disturbed")
	}
	closeBounded(t, u)
}

func TestReinviteAnswerCarriesRegisteredContact(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	newFakeRegistrar(tr, 0)
	dialogs := make(chan *siptest.Dialog, 1)
	tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
		if d, err := tr.Accept(req, tx, answerBody()); err == nil {
			dialogs <- d
		}
	})
	u := regUA(t, tr, nil)
	r, log, _ := startReg(t, u, tr, 60*time.Second)
	waitState(t, log, "registered", registered)
	call := result(t, start(t, u, dialRequest(tr, nil)), 5*time.Second)
	if call.Call == nil {
		t.Fatalf("result = %+v", call)
	}
	d := <-dialogs
	res, err := d.ReInvite(ctx5(t), reofferBody(30000, ""))
	if err != nil || res.StatusCode != 200 || res.Contact() == nil {
		t.Fatalf("re-INVITE = %v, %v", res, err)
	}
	c := res.Contact().Address
	want := r.State().Contact
	if c.User != testUser || c.Host != want.Addr().String() || c.Port != int(want.Port()) {
		t.Errorf("200 Contact = %v, want the registered %s@%v", c.String(), testUser, want)
	}
	closeBounded(t, u)
}

func TestRetransmittedReinviteIsAbsorbed(t *testing.T) {
	tr, u, _, d := establish(t, "udp", nil)
	src := learnFlow(t, tr, u)
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	local := pc.LocalAddr().(*net.UDPAddr)
	req := d.FarEndRequest("INVITE", 77)
	req.AppendHeader(&gosip.ViaHeader{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP",
		Host: "127.0.0.1", Port: local.Port, Params: gosip.HeaderParams{{K: "branch", V: "z9hG4bKretrans1"}, {K: "rport", V: ""}}})
	req.AppendHeader(&gosip.ContactHeader{Address: gosip.Uri{Scheme: "sip", User: "trunk", Host: "127.0.0.1", Port: local.Port}})
	req.AppendHeader(gosip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(reofferBody(30000, ""))
	wire := []byte(req.String())

	send := func() {
		if _, err := pc.WriteTo(wire, net.UDPAddrFromAddrPort(src)); err != nil {
			t.Fatal(err)
		}
	}
	send()
	send() // back to back
	time.Sleep(300 * time.Millisecond)
	send() // after the first answer
	statuses := map[int]int{}
	buf := make([]byte, 4096)
	pc.SetReadDeadline(time.Now().Add(800 * time.Millisecond))
	for {
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			break
		}
		var res gosip.Response
		if m, perr := gosip.ParseMessage(buf[:n]); perr == nil {
			if r, ok := m.(*gosip.Response); ok {
				res = *r
				statuses[res.StatusCode]++
			}
		}
	}
	if statuses[500] != 0 {
		t.Errorf("a retransmitted re-INVITE was answered 500: %v", statuses)
	}
	if statuses[200] == 0 {
		t.Errorf("no 200 seen: %v", statuses)
	}
	closeBounded(t, u)
}
