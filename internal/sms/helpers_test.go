package sms

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/btsdp"
	"github.com/volodymyrsmirnov/siptooth/internal/obex"
)

const waitFor = 5 * time.Second

// eventually polls cond until it holds or the deadline passes (an observable condition, never a
// replacement for ordering).
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// ---- fake clock ----

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	c      *fakeClock
	at     time.Time
	d      time.Duration
	ch     chan time.Time
	active bool
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, at: c.now.Add(d), d: d, ch: make(chan time.Time, 1), active: true}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.active = false
	return was
}

// Advance moves time forward and fires every timer that became due, earliest first.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	var due []*fakeTimer
	for _, t := range c.timers {
		if t.active && !t.at.After(c.now) {
			due = append(due, t)
		}
	}
	sort.SliceStable(due, func(i, j int) bool { return due[i].at.Before(due[j].at) })
	for _, t := range due {
		t.active = false
		t.ch <- c.now
	}
}

// pending lists the durations of the armed timers.
func (c *fakeClock) pending() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []time.Duration
	for _, t := range c.timers {
		if t.active {
			out = append(out, t.d)
		}
	}
	return out
}

func (c *fakeClock) has(d time.Duration) bool {
	for _, p := range c.pending() {
		if p == d {
			return true
		}
	}
	return false
}

// waitTimer blocks until a timer of duration d is armed (so Advance never races its creation).
func (c *fakeClock) waitTimer(t *testing.T, d time.Duration) {
	t.Helper()
	eventually(t, fmt.Sprintf("timer %v (pending %v)", d, c.pending()), func() bool { return c.has(d) })
}

// ---- fake sink ----

type fakeSink struct {
	mu      sync.Mutex
	msgs    []Message
	unavail []string
	reject  bool
}

func (s *fakeSink) Deliver(m Message) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, m)
	return !s.reject
}

func (s *fakeSink) Unavailable(d string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unavail = append(s.unavail, d)
}

func (s *fakeSink) messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.msgs...)
}

func (s *fakeSink) unavailable() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.unavail...)
}

// ---- fake MSE (the phone's message access server) ----

type fakeMSE struct {
	c              *obex.Conn
	connectCode    byte             // 0: Success
	connectGate    chan struct{}    // non-nil: Connect is answered only when closed
	regGate        chan struct{}    // non-nil: registration is answered only when closed
	regFn          func(n int) byte // response code for the n-th registration (1-based); nil: Success
	getFn          func(handle string) obex.Packet
	closeOnConnect bool

	gotConnect chan struct{}
	regs       chan struct{}
	gets       chan string
	done       chan struct{}
}

func startMSE(t *testing.T, rwc io.ReadWriteCloser, cfg func(*fakeMSE)) *fakeMSE {
	m := &fakeMSE{
		c:          obex.NewConn(rwc, 1<<16),
		gotConnect: make(chan struct{}, 4), regs: make(chan struct{}, 64), gets: make(chan string, 256), done: make(chan struct{}),
	}
	if cfg != nil {
		cfg(m)
	}
	t.Cleanup(func() { m.c.Close() })
	go m.serve()
	return m
}

func (m *fakeMSE) send(p obex.Packet) {
	b, err := p.Marshal()
	if err == nil {
		_ = m.c.Write(b)
	}
}

func (m *fakeMSE) serve() {
	defer close(m.done)
	nreg := 0
	for {
		b, err := m.c.Next(0)
		if err != nil {
			return
		}
		p, err := obex.ParseRequest(b)
		if err != nil {
			return
		}
		switch p.Code {
		case obex.OpConnect:
			m.gotConnect <- struct{}{}
			if m.closeOnConnect {
				m.c.Close()
				return
			}
			if m.connectGate != nil {
				select {
				case <-m.connectGate:
				case <-m.c.Done():
					return
				}
			}
			if m.connectCode != 0 {
				m.send(obex.Packet{Code: m.connectCode})
				continue
			}
			m.send(obex.Packet{Code: obex.RespSuccess, Connect: &obex.ConnectFields{Version: 0x10, MaxPacket: obex.MaxPacket},
				Headers: obex.Headers{obex.U32(obex.HConnectionID, 7), obex.Bytes(obex.HWho, MASTarget)}})
		case obex.OpPutFinal:
			nreg++
			m.regs <- struct{}{}
			if m.regGate != nil {
				select {
				case <-m.regGate:
				case <-m.c.Done():
					return
				}
			}
			code := byte(obex.RespSuccess)
			if m.regFn != nil {
				code = m.regFn(nreg)
			}
			m.send(obex.Packet{Code: code})
		case obex.OpGetFinal:
			v, _ := p.Headers.Get(obex.HName)
			h, _ := obex.DecodeUnicode(v)
			m.gets <- h
			resp := obex.Packet{Code: obex.RespNotFound}
			if m.getFn != nil {
				resp = m.getFn(h)
			}
			m.send(resp)
		case obex.OpDisconnect:
			m.send(obex.Packet{Code: obex.RespSuccess})
			return
		}
	}
}

func bmsgText(from, name, text string) string {
	card := "BEGIN:VCARD\r\nVERSION:2.1\r\n"
	if name != "" {
		card += "FN:" + name + "\r\n"
	}
	card += "TEL:" + from + "\r\nEND:VCARD\r\n"
	return "BEGIN:BMSG\r\nVERSION:1.0\r\nSTATUS:UNREAD\r\nTYPE:SMS_GSM\r\nFOLDER:TELECOM/MSG/INBOX\r\n" + card +
		"BEGIN:BENV\r\nBEGIN:BBODY\r\nCHARSET:UTF-8\r\nLENGTH:20\r\nBEGIN:MSG\r\n" + text + "\r\nEND:MSG\r\nEND:BBODY\r\nEND:BENV\r\nEND:BMSG\r\n"
}

// getOK answers a GetMessage with a message whose text is the handle.
func getOK(h string) obex.Packet {
	return getBody(bmsgText("+380501234567", "Jane", "text of "+h))
}

func getBody(s string) obex.Packet {
	return obex.Packet{Code: obex.RespSuccess, Headers: obex.Headers{obex.Bytes(obex.HEndOfBody, []byte(s))}}
}

// ---- fake dialer ----

type fakeDialer struct {
	t    *testing.T
	find func(ctx context.Context) (btsdp.MASRecord, error)
	dial func(ctx context.Context, ch uint8) (io.ReadWriteCloser, error)
	cfg  func(*fakeMSE)
	mses chan *fakeMSE
}

func (d *fakeDialer) FindMAS(ctx context.Context) (btsdp.MASRecord, error) {
	if d.find != nil {
		return d.find(ctx)
	}
	return btsdp.MASRecord{Channel: 5, InstanceID: 0, MessageTypes: btsdp.MsgTypeSMSGSM}, nil
}

func (d *fakeDialer) Dial(ctx context.Context, ch uint8) (io.ReadWriteCloser, error) {
	if d.dial != nil {
		return d.dial(ctx, ch)
	}
	a, b := net.Pipe()
	d.mses <- startMSE(d.t, b, d.cfg)
	return a, nil
}

// ---- log capture ----

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// ---- rig ----

type rig struct {
	t      *testing.T
	clock  *fakeClock
	sink   *fakeSink
	dialer *fakeDialer
	logs   *syncBuf
	svc    *Service
	cancel context.CancelFunc
	done   chan struct{}
	stuck  atomic.Int32
	ids    atomic.Int32
}

// newRig builds a stopped rig; start runs the service.
func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, clock: newFakeClock(), sink: &fakeSink{}, logs: &syncBuf{}, done: make(chan struct{})}
	r.dialer = &fakeDialer{t: t, mses: make(chan *fakeMSE, 64)}
	logger := slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r.svc = NewService(Deps{
		Enabled: true, Sink: r.sink, Clock: r.clock, Logger: logger,
		NewID:    func(time.Time) string { return fmt.Sprintf("id-%d", r.ids.Add(1)) },
		MNSStuck: func() { r.stuck.Add(1) },
	})
	return r
}

func (r *rig) start() {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() { r.svc.Run(ctx); close(r.done) }()
	r.t.Cleanup(r.stop)
}

func (r *rig) stop() {
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(waitFor):
		r.t.Error("Run did not return")
	}
}

func startedRig(t *testing.T) *rig {
	r := newRig(t)
	r.start()
	return r
}

func (r *rig) waitState(want string) {
	r.t.Helper()
	eventually(r.t, "state "+want+" (now "+r.svc.Status().State+")", func() bool { return r.svc.Status().State == want })
}

func (r *rig) nextMSE() *fakeMSE {
	r.t.Helper()
	select {
	case m := <-r.dialer.mses:
		return m
	case <-time.After(waitFor):
		r.t.Fatal("no MAS connection was dialled")
		return nil
	}
}

func waitCh[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitFor):
		t.Fatalf("timed out waiting for %s", what)
		var z T
		return z
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(waitFor):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// attempt sends the phone up (or waits for a scheduled retry of d) and returns the MSE of the
// resulting connection once its registration request has arrived and been answered.
func (r *rig) up() *fakeMSE {
	r.t.Helper()
	r.svc.PhoneLink(true, r.dialer)
	return r.attemptAfter(settleDelay)
}

func (r *rig) attemptAfter(d time.Duration) *fakeMSE {
	r.t.Helper()
	r.clock.waitTimer(r.t, d)
	r.clock.Advance(d)
	mse := r.nextMSE()
	waitCh(r.t, mse.regs, "registration request")
	r.clock.waitTimer(r.t, mnsWindow)
	return mse
}

// mnsPhone is the phone's side of an MNS connection.
type mnsPhone struct{ *fakeEnd }

func (r *rig) connectMNS() *mnsPhone {
	r.t.Helper()
	a, b := net.Pipe()
	r.svc.MNSConnection(a)
	f := newFakeEnd(r.t, b)
	f.mnsConnect()
	return &mnsPhone{f}
}

func (p *mnsPhone) event(handle string) byte {
	return p.putEvent(typeEventReport, nil, evXML(handle))
}

func evXML(handle string) string {
	return `<MAP-event-report version="1.0"><event type="NewMessage" handle="` + handle + `" folder="TELECOM/MSG/INBOX" msg_type="SMS_GSM"/></MAP-event-report>`
}

// ready brings the service to ready.
func (r *rig) ready() (*fakeMSE, *mnsPhone) {
	r.t.Helper()
	mse := r.up()
	p := r.connectMNS()
	r.waitState(StateReady)
	return mse, p
}

func (r *rig) logged(sub string) bool { return strings.Contains(r.logs.String(), sub) }
