package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/g711"
	"github.com/volodymyrsmirnov/siptooth/internal/hfp"
	"github.com/volodymyrsmirnov/siptooth/internal/hooks"
	"github.com/volodymyrsmirnov/siptooth/internal/nat"
	"github.com/volodymyrsmirnov/siptooth/internal/rtp"
	"github.com/volodymyrsmirnov/siptooth/internal/sco"
	"github.com/volodymyrsmirnov/siptooth/internal/sdp"
	"github.com/volodymyrsmirnov/siptooth/internal/sip"
)

// --- telephony ---

type fakeTel struct {
	events chan hfp.Event
	mu     sync.Mutex
	answer int
	hangup int
	// answerErr is what Answer returns; gate, when set, holds Answer until closed.
	answerErr error
	gate      chan struct{}
	// hangupGate, when set, holds Hangup like a slow command queue.
	hangupGate chan struct{}
	// ignoreCtx makes a gated Answer behave as already written: only the gate ends it.
	ignoreCtx bool
}

func newFakeTel() *fakeTel { return &fakeTel{events: make(chan hfp.Event, 32)} }

func (f *fakeTel) Events() <-chan hfp.Event { return f.events }
func (f *fakeTel) Answer(ctx context.Context) error {
	f.mu.Lock()
	f.answer++
	err, gate := f.answerErr, f.gate
	f.mu.Unlock()
	if gate != nil {
		if f.ignoreCtx { // the command is already on the wire
			<-gate
			return err
		}
		select {
		case <-gate:
		case <-ctx.Done(): // like hfp: a command cancelled before it was written was not sent
			return fmt.Errorf("%w: %v", hfp.ErrNotSent, ctx.Err())
		}
	}
	return err
}
func (f *fakeTel) setAnswerErr(err error) { f.mu.Lock(); f.answerErr = err; f.mu.Unlock() }

// Hangup counts only commands that reach the "wire": with hangupGate set it
// waits for the gate, and a cancelled context aborts it unsent.
func (f *fakeTel) Hangup(ctx context.Context) error {
	f.mu.Lock()
	gate := f.hangupGate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", hfp.ErrNotSent, ctx.Err())
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hangup++
	return nil
}
func (f *fakeTel) send(kind hfp.EventKind, number string) {
	f.events <- hfp.Event{Kind: kind, Number: number}
}
func (f *fakeTel) counts() (answers, hangups int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.answer, f.hangup
}

// --- media ---

type fakeMedia struct {
	mu           sync.Mutex
	closed       int
	stats        rtp.Stats
	started      []sdp.Media
	keys         [][]byte
	startErr     error
	silences     int
	updates      []sdp.Media
	errCh        chan error
	pl           *rtp.Playout
	panicOn      bool // Stats panics
	panicSilence bool // WriteSilence panics
	panicFrame   bool // WriteFrame panics
	frames       int
	writeErr     error
}

func newFakeMedia() *fakeMedia {
	return &fakeMedia{errCh: make(chan error, 1), pl: rtp.NewPlayout(rtp.Config{JitterMs: 60})}
}

func (m *fakeMedia) Start(r sdp.Media, key []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = append(m.started, r)
	m.keys = append(m.keys, key)
	return m.startErr
}
func (m *fakeMedia) Update(r sdp.Media) { m.mu.Lock(); m.updates = append(m.updates, r); m.mu.Unlock() }
func (m *fakeMedia) WriteFrame([]int16) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.panicFrame {
		panic("frame exploded")
	}
	m.frames++
	return m.writeErr
}
func (m *fakeMedia) WriteSilence() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.panicSilence {
		panic("silence exploded")
	}
	m.silences++
	return m.writeErr
}
func (m *fakeMedia) Playout() *rtp.Playout { return m.pl }
func (m *fakeMedia) Err() <-chan error     { return m.errCh }
func (m *fakeMedia) Stats() rtp.Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.panicOn {
		panic("stats exploded")
	}
	return m.stats
}
func (m *fakeMedia) Close() error    { m.mu.Lock(); m.closed++; m.mu.Unlock(); return nil }
func (m *fakeMedia) closeCount() int { m.mu.Lock(); defer m.mu.Unlock(); return m.closed }
func (m *fakeMedia) silenceCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.silences
}
func (m *fakeMedia) frameCount() int { m.mu.Lock(); defer m.mu.Unlock(); return m.frames }
func (m *fakeMedia) setLastRx(t time.Time) {
	m.mu.Lock()
	m.stats.LastRx = t
	m.mu.Unlock()
}
func (m *fakeMedia) updateCount() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.updates) }
func (m *fakeMedia) startCount() int  { m.mu.Lock(); defer m.mu.Unlock(); return len(m.started) }

// --- SIP ---

type fakeCall struct {
	updates     chan sdp.Media
	panicRemote bool
	ended       chan sip.EndCause
	mu          sync.Mutex
	hangup      int
	// hangupGate, when set, holds Hangup (a BYE in flight) until closed.
	hangupGate chan struct{}
}

func newFakeCall() *fakeCall {
	return &fakeCall{ended: make(chan sip.EndCause, 1), updates: make(chan sdp.Media, 4)}
}

func (c *fakeCall) setPanicRemote() { c.mu.Lock(); c.panicRemote = true; c.mu.Unlock() }

func (c *fakeCall) Remote() sdp.Media {
	c.mu.Lock()
	p := c.panicRemote
	c.mu.Unlock()
	if p {
		panic("remote exploded")
	}
	return sdp.Media{Addr: netip.MustParseAddrPort("203.0.113.9:4000"), SendCodec: g711.PCMU}
}
func (c *fakeCall) MediaUpdates() <-chan sdp.Media { return c.updates }
func (c *fakeCall) Ended() <-chan sip.EndCause     { return c.ended }
func (c *fakeCall) Hangup(ctx context.Context) error {
	c.mu.Lock()
	gate := c.hangupGate
	c.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hangup++
	return nil
}
func (c *fakeCall) SIPCallID() string         { return "sip-call-1" }
func (c *fakeCall) LastStatus() (int, string) { return 200, "OK" }
func (c *fakeCall) hangups() int              { c.mu.Lock(); defer c.mu.Unlock(); return c.hangup }

// fakeAttempt is stricter than the real one: Abandon only counts, so a test
// decides if and when a result arrives.
type fakeAttempt struct {
	result    chan sip.DialResult
	mu        sync.Mutex
	abandoned int
}

func newFakeAttempt() *fakeAttempt { return &fakeAttempt{result: make(chan sip.DialResult, 1)} }

func (a *fakeAttempt) Result() <-chan sip.DialResult { return a.result }
func (a *fakeAttempt) Abandon()                      { a.mu.Lock(); a.abandoned++; a.mu.Unlock() }
func (a *fakeAttempt) abandons() int                 { a.mu.Lock(); defer a.mu.Unlock(); return a.abandoned }
func (a *fakeAttempt) send(r sip.DialResult)         { a.result <- r }

type fakeSIP struct {
	mu         sync.Mutex
	hosts      []string
	reqs       []sip.DialRequest
	attempts   []*fakeAttempt
	medias     []*fakeMedia
	prepareErr error
	startErr   error
	// gate, when set, makes Prepare wait for it (ignoring ctx, like a slow
	// socket setup that finishes after the call is over).
	gate chan struct{}
	// panicFirstStats makes the first media panic in Stats.
	panicFirstStats bool
	// panicSilenceFirst makes the first media panic in WriteSilence.
	panicSilenceFirst bool
	localKey          []byte
	mediaStartErr     error
}

func (s *fakeSIP) Prepare(ctx context.Context, host string) (Prepared, error) {
	s.mu.Lock()
	s.hosts = append(s.hosts, host)
	gate, perr := s.gate, s.prepareErr
	s.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if perr != nil {
		return Prepared{}, perr
	}
	m := newFakeMedia()
	s.mu.Lock()
	m.panicOn = s.panicFirstStats && len(s.medias) == 0
	m.panicSilence = s.panicSilenceFirst && len(s.medias) == 0
	s.medias = append(s.medias, m)
	s.mu.Unlock()
	m.startErr = s.mediaStartErr
	return Prepared{
		Media:    m,
		LocalKey: s.localKey,
		Offer:    sdp.Offer{Addr: netip.MustParseAddrPort("192.0.2.10:10000"), Codecs: []g711.Codec{g711.PCMU}},
		Contact:  "sip:siptooth@192.0.2.10:5061;transport=tls",
		NAT:      nat.Decision{Mode: nat.ModeLocal, Media: netip.MustParseAddrPort("192.0.2.10:10000")},
	}, nil
}

func (s *fakeSIP) Start(req sip.DialRequest) (sip.Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startErr != nil {
		return nil, s.startErr
	}
	a := newFakeAttempt()
	s.reqs = append(s.reqs, req)
	s.attempts = append(s.attempts, a)
	return a, nil
}

func (s *fakeSIP) starts() int   { s.mu.Lock(); defer s.mu.Unlock(); return len(s.reqs) }
func (s *fakeSIP) prepares() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.hosts) }
func (s *fakeSIP) req(i int) sip.DialRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqs[i]
}
func (s *fakeSIP) attempt(i int) *fakeAttempt { s.mu.Lock(); defer s.mu.Unlock(); return s.attempts[i] }
func (s *fakeSIP) media(i int) *fakeMedia     { s.mu.Lock(); defer s.mu.Unlock(); return s.medias[i] }
func (s *fakeSIP) mediaCount() int            { s.mu.Lock(); defer s.mu.Unlock(); return len(s.medias) }

// --- hooks ---

type hookEvent struct {
	event string
	info  hooks.CallInfo
}

type fakeHooks struct {
	mu     sync.Mutex
	events []hookEvent
}

func (h *fakeHooks) Emit(event string, c hooks.CallInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, hookEvent{event, c})
}

func (h *fakeHooks) all() []hookEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hookEvent(nil), h.events...)
}

func (h *fakeHooks) named(event string) []hooks.CallInfo {
	var out []hooks.CallInfo
	for _, e := range h.all() {
		if e.event == event {
			out = append(out, e.info)
		}
	}
	return out
}

func (h *fakeHooks) names() []string {
	var out []string
	for _, e := range h.all() {
		out = append(out, e.event)
	}
	return out
}

// --- log capture ---

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// --- SCO ---

// fakeListener hands out links pushed with offer; Accept honors ctx.
type fakeListener struct {
	links    chan sco.Link
	lateLink sco.Link // returned when ctx ends
	mu       sync.Mutex
	accepts  int
}

func newFakeListener() *fakeListener { return &fakeListener{links: make(chan sco.Link, 4)} }

func (l *fakeListener) Accept(ctx context.Context) (sco.Link, error) {
	l.mu.Lock()
	l.accepts++
	l.mu.Unlock()
	select {
	case k := <-l.links:
		return k, nil
	case <-ctx.Done():
		if l.lateLink != nil { // accepted just as the call ended
			return l.lateLink, nil
		}
		return nil, ctx.Err()
	}
}
func (l *fakeListener) Close() error { return nil }
func (l *fakeListener) acceptCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accepts
}

type fakeDialer struct {
	links chan sco.Link
	mu    sync.Mutex
	dials []string
}

func newFakeDialer() *fakeDialer { return &fakeDialer{links: make(chan sco.Link, 4)} }

func (d *fakeDialer) Dial(ctx context.Context, adapter, remote string) (sco.Link, error) {
	d.mu.Lock()
	d.dials = append(d.dials, adapter+">"+remote)
	d.mu.Unlock()
	select {
	case k := <-d.links:
		return k, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (d *fakeDialer) dialed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dials...)
}

// --- harness ---

type rig struct {
	t      *testing.T
	c      *Coordinator
	clock  *fakeClock
	tel    *fakeTel
	lis    *fakeListener
	dialer *fakeDialer
	sip    *fakeSIP
	hooks  *fakeHooks
	log    *syncBuffer
	cancel context.CancelFunc
	done   chan error
	cfg    config.Config
}

func testConfig() config.Config {
	cfg := config.Default()
	cfg.Phone.Number = "+380441234567"
	cfg.Bluetooth.PhoneAddress = "11:11:11:11:11:11"
	cfg.SIP.Target = "sip:{number}@sip.example.com;transport=tls"
	cfg.SIP.Headers = map[string]string{"X-CALL-ID": "{call_id}"}
	return cfg
}

// newRig builds a coordinator with a phone attached and routing OK, but does
// not start Run.
func newRig(t *testing.T, mutate func(*config.Config)) *rig {
	t.Helper()
	cfg := testConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	r := &rig{t: t, clock: newFakeClock(), tel: newFakeTel(), lis: newFakeListener(), dialer: newFakeDialer(), sip: &fakeSIP{}, hooks: &fakeHooks{}, log: &syncBuffer{}, cfg: cfg}
	n := 0
	r.c = New(Deps{
		Config: cfg, SIP: r.sip, Dialer: r.dialer, Hooks: r.hooks, Clock: r.clock,
		Logger: slog.New(slog.NewTextHandler(io.MultiWriter(r.log), &slog.HandlerOptions{Level: slog.LevelDebug})),
		NewCallID: func(time.Time) string {
			n++
			return "call-" + string(rune('0'+n))
		},
	})
	return r
}

// start runs the coordinator and attaches the phone with routing OK.
func (r *rig) start() *rig {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan error, 1)
	go func() { r.done <- r.c.Run(ctx) }()
	r.c.Attach(r.tel, r.lis, "AA:BB:CC:DD:EE:FF")
	r.c.SetAudioRouting(true)
	r.waitFor("ready", func() bool { return r.c.Status().Ready })
	r.t.Cleanup(r.stop)
	return r
}

// stop cancels Run and waits for it; safe to call twice.
func (r *rig) stop() {
	r.cancel()
	select {
	case <-r.done:
		r.done <- nil
	case <-time.After(3 * time.Second):
		r.t.Error("Run did not return")
	}
}

func (r *rig) waitFor(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitArmed waits until the coordinator has created n clock timers.
func (r *rig) waitArmed(n int) {
	r.t.Helper()
	r.waitFor("timers armed", func() bool { return r.clock.armed() >= n })
}

func (r *rig) waitState(s string) {
	r.t.Helper()
	r.waitFor("state "+s, func() bool { return r.c.Status().State == s })
}

// waitEnded waits for the call.ended hook and returns it.
func (r *rig) waitEnded() hooks.CallInfo {
	r.t.Helper()
	r.waitFor("call.ended", func() bool { return len(r.hooks.named("call.ended")) > 0 })
	r.waitState("idle")
	return r.hooks.named("call.ended")[0]
}

// settle gives the loop time to process what was sent; used only before
// asserting that nothing happened.
func (r *rig) settle() { time.Sleep(40 * time.Millisecond) }

// ring rings the phone with a caller ID that arrives immediately and waits
// for the INVITE.
func (r *rig) ringAndDial(caller string) {
	r.t.Helper()
	r.tel.send(hfp.Ring, "")
	r.tel.send(hfp.CallerID, caller)
	r.waitFor("INVITE", func() bool { return r.sip.starts() == 1 })
}

var errBoom = errors.New("boom")
