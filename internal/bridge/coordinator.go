// Package bridge owns the life of one phone call: the coordinator state
// machine (spec 4) and the SCO to RTP media pump (spec 6.2).
package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/hfp"
	"github.com/volodymyrsmirnov/siptooth/internal/hooks"
	"github.com/volodymyrsmirnov/siptooth/internal/nat"
	"github.com/volodymyrsmirnov/siptooth/internal/rtp"
	"github.com/volodymyrsmirnov/siptooth/internal/sco"
	"github.com/volodymyrsmirnov/siptooth/internal/sdp"
	"github.com/volodymyrsmirnov/siptooth/internal/sip"
)

// Telephony is the HFP client as the coordinator uses it (*hfp.Client).
type Telephony interface {
	Events() <-chan hfp.Event
	Answer(ctx context.Context) error
	Hangup(ctx context.Context) error
}

// Media is the RTP session of one call. The adapter in the daemon wraps
// *rtp.Session. Close must be safe to call on a session that was never
// started.
type Media interface {
	Start(m sdp.Media, localKey []byte) error
	Update(m sdp.Media)
	WriteFrame(pcm []int16) error
	WriteSilence() error
	Playout() *rtp.Playout
	Stats() rtp.Stats
	Err() <-chan error
	Close() error
}

// Prepared is what SIP.Prepare produces for one call: the open RTP socket,
// the advertised addresses and the INVITE ingredients that depend on them.
type Prepared struct {
	Media     Media
	Offer     sdp.Offer
	LocalKey  []byte // SDES key sent in the offer; nil for plain RTP
	Contact   string // signaling address URI (spec 7.3)
	NAT       nat.Decision
	NextHop   string // "host:port" the INVITE goes to; empty: sip.proxy or the UA default
	Transport string // "udp" | "tcp" | "tls"; empty: the UA default
}

// SIP starts outgoing calls.
type SIP interface {
	// Prepare opens the RTP socket, decides the advertised addresses toward
	// nextHopHost and works out the Contact (spec 4.2 step 4). It must honor
	// ctx (the call ended) and return promptly after it is cancelled. A STUN
	// failure is not an error (spec 7.2). On error the Media, if any, is
	// closed by the coordinator.
	Prepare(ctx context.Context, nextHopHost string) (Prepared, error)
	// Start sends the INVITE and returns at once.
	Start(req sip.DialRequest) (sip.Attempt, error)
}

// HookSink receives lifecycle events; Emit never blocks (*hooks.Dispatcher).
type HookSink interface {
	Emit(event string, c hooks.CallInfo)
}

// Deps are the coordinator's collaborators.
type Deps struct {
	Config    config.Config
	SIP       SIP
	Dialer    sco.Dialer
	Hooks     HookSink
	Logger    *slog.Logger
	Clock     Clock
	NewCallID func(time.Time) string
}

// cleanupTimeout bounds each background BYE and AT+CHUP (spec 4.6).
const cleanupTimeout = 3 * time.Second

// defaultDialTimeout applies when the config carries none.
const defaultDialTimeout = 30 * time.Second

type msgKind int

const (
	mTel msgKind = iota + 1
	mAttach
	mDetach
	mRouting
	mRegistration
	mTimer
	mPrepared
	mDial
	mSIPEnded
)

type msg struct {
	kind msgKind
	gen  int // call generation (mTimer, mPrepared, mDial, mSIPEnded)

	tel    hfp.Event
	telGen int
	telRef Telephony
	lis    sco.Listener
	addr   string

	ok  bool               // mRouting
	reg RegistrationStatus // mRegistration

	timer    string // mTimer
	timerGen int

	prepared Prepared // mPrepared
	err      error
	result   sip.DialResult // mDial
	cause    sip.EndCause   // mSIPEnded
}

// Coordinator is the single owner of call state. Everything is mutated on the
// Run goroutine; the exported methods only post messages or read a snapshot.
type Coordinator struct {
	d     Deps
	inbox chan msg
	done  chan struct{} // closed when Run stops accepting messages
	bg    sync.WaitGroup

	// loop-owned
	tel          Telephony
	listener     sco.Listener
	adapterAddr  string
	telGen       int
	routingOK    bool
	registration RegistrationStatus
	lastOutcome  string
	nextGen      int
	cur          *activeCall

	statusMu sync.RWMutex
	status   Status
}

// New returns a coordinator; call Run to start it.
func New(d Deps) *Coordinator {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Clock == nil {
		d.Clock = RealClock{}
	}
	if d.NewCallID == nil {
		d.NewCallID = newCallID
	}
	if d.Hooks == nil {
		d.Hooks = nopHooks{}
	}
	return &Coordinator{d: d, inbox: make(chan msg, 256), done: make(chan struct{}), status: Status{State: stateIdle.String()}}
}

type nopHooks struct{}

func (nopHooks) Emit(string, hooks.CallInfo) {}

// newCallID is the default call ID: 20261014T094331Z-b5fa.
func newCallID(t time.Time) string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return t.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// post delivers m to the loop; it reports false once Run stopped accepting.
func (c *Coordinator) post(m msg) bool {
	select {
	case c.inbox <- m:
		return true
	case <-c.done:
		return false
	}
}

// ---- public API (thread-safe) ----

// Attach hands over a connected HFP client, the SCO listener and the local
// adapter address used for dialing SCO.
func (c *Coordinator) Attach(t Telephony, l sco.Listener, adapterAddress string) {
	c.post(msg{kind: mAttach, telRef: t, lis: l, addr: adapterAddress})
}

// Detach drops the phone: a call in progress ends as bluetooth_lost.
func (c *Coordinator) Detach() { c.post(msg{kind: mDetach}) }

// SetAudioRouting reports whether the SCO routing is verified; false makes
// the coordinator ignore rings (the phone just rings).
func (c *Coordinator) SetAudioRouting(ok bool) { c.post(msg{kind: mRouting, ok: ok}) }

// SetRegistration records the SIP registration state for Status.
func (c *Coordinator) SetRegistration(s RegistrationStatus) {
	c.post(msg{kind: mRegistration, reg: s})
}

// Status returns the latest published snapshot.
func (c *Coordinator) Status() Status {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()
	return c.status
}

// Run processes events until ctx is done, then ends a call in progress as
// shutdown and waits for the background hangups (each bounded by 3 s).
func (c *Coordinator) Run(ctx context.Context) error {
	c.publishStatus()
	for {
		select {
		case <-ctx.Done():
			c.shutdown()
			return ctx.Err()
		case m := <-c.inbox:
			c.handle(ctx, m)
		}
	}
}

func (c *Coordinator) shutdown() {
	if c.cur != nil {
		c.finish(c.cur, endSpec{outcome: OutcomeShutdown, hangupBy: HangupSiptooth})
	}
	close(c.done)
	// Helper goroutines that hold a resource (Prepare) post or release it
	// themselves; once they are done nothing else can enter the inbox
	// meaningfully, so what is left is released here.
	c.bg.Wait()
	for {
		select {
		case m := <-c.inbox:
			c.discard(m, "coordinator stopped")
		default:
			c.bg.Wait() // BYEs started by discarded late answers
			c.publishStatus()
			return
		}
	}
}

func (c *Coordinator) handle(ctx context.Context, m msg) {
	switch m.kind {
	case mAttach:
		c.telGen++
		c.tel, c.listener, c.adapterAddr = m.telRef, m.lis, m.addr
		gen := c.telGen
		go func(t Telephony) {
			// hfp delivers losslessly, so keep draining for the client's whole
			// life; events of a stale generation are dropped in the loop.
			for ev := range t.Events() {
				if !c.post(msg{kind: mTel, tel: ev, telGen: gen}) {
					return
				}
			}
		}(m.telRef)
	case mDetach:
		c.dropPhone()
	case mTel:
		if m.telGen != c.telGen {
			return
		}
		c.onTel(ctx, m.tel)
	case mRouting:
		c.routingOK = m.ok
	case mRegistration:
		c.registration = m.reg
	case mTimer:
		c.onTimer(m)
	case mPrepared:
		c.onPrepared(m)
	case mDial:
		c.onDialResult(m)
	case mSIPEnded:
		c.onSIPEnded(m)
	}
	c.publishStatus()
}

// current returns the call a generation-tagged message belongs to, or nil
// when the call is over or a newer one runs (the message is then late).
func (c *Coordinator) current(gen int) *activeCall {
	if c.cur == nil || c.cur.gen != gen || c.cur.state == stateEnding {
		return nil
	}
	return c.cur
}

// discard releases what a late message carries and logs it (spec 4.1).
func (c *Coordinator) discard(m msg, why string) {
	switch m.kind {
	case mPrepared:
		closeMedia(c.d.Logger, m.prepared.Media)
		c.d.Logger.Info("late prepare result discarded", "why", why, "gen", m.gen)
	case mDial:
		if m.result.Call != nil {
			c.d.Logger.Warn("late SIP answer discarded, hanging up", "why", why, "sip_call_id", m.result.Call.SIPCallID())
			c.hangupSIP(m.result.Call, "")
		} else {
			c.d.Logger.Info("late dial result discarded", "why", why, "cause", m.result.Cause)
		}
	case mTimer:
		c.d.Logger.Debug("late timer discarded", "timer", m.timer)
	default:
		c.d.Logger.Debug("late message discarded", "why", why, "kind", int(m.kind))
	}
}

func (c *Coordinator) publishStatus() {
	st := Status{
		PhoneConnected: c.tel != nil,
		Ready:          c.ready(),
		State:          stateIdle.String(),
		LastOutcome:    c.lastOutcome,
		Registration:   c.registration,
	}
	if call := c.cur; call != nil {
		st.State, st.CallID, st.Caller = call.state.String(), call.id, call.caller
		t := call.ringAt
		st.CallStartedAt = &t
	}
	c.statusMu.Lock()
	c.status = st
	c.statusMu.Unlock()
}

func (c *Coordinator) ready() bool { return c.tel != nil && c.routingOK }

// ---- timers ----

// startTimer arms a named timer of call; re-arming or stopTimer invalidates
// an earlier arming through the generation counter.
func (c *Coordinator) startTimer(call *activeCall, name string, d time.Duration) {
	call.timerGen[name]++
	gen := call.timerGen[name]
	t := c.d.Clock.NewTimer(d)
	go func() {
		select {
		case <-t.C():
			c.post(msg{kind: mTimer, gen: call.gen, timer: name, timerGen: gen})
		case <-call.ctx.Done():
			t.Stop()
		}
	}()
}

func (c *Coordinator) stopTimer(call *activeCall, name string) { call.timerGen[name]++ }

func (c *Coordinator) onTimer(m msg) {
	call := c.current(m.gen)
	if call == nil || call.timerGen[m.timer] != m.timerGen {
		c.discard(m, "stale timer")
		return
	}
	switch m.timer {
	case timerCallerID:
		c.onCallerIDWait(call)
	case timerDial:
		c.onDialTimeout(call)
	}
}

// ---- telephony events ----

// dropPhone forgets the HFP client; a call in progress ends as bluetooth_lost.
func (c *Coordinator) dropPhone() {
	c.tel, c.listener = nil, nil
	c.telGen++
	if c.cur != nil {
		c.finish(c.cur, endSpec{outcome: OutcomeBluetoothLost, hangupBy: HangupSiptooth})
	}
}

func (c *Coordinator) onTel(ctx context.Context, ev hfp.Event) {
	switch ev.Kind {
	case hfp.Connected:
		// PhoneConnected follows Attach.
	case hfp.Disconnected:
		c.dropPhone()
	case hfp.Ring:
		if c.cur != nil {
			// Call waiting or a repeated RING: the call in progress is not touched.
			c.d.Logger.Debug("ring while busy ignored", "call_id", c.cur.id, "state", c.cur.state.String())
			return
		}
		if !c.ready() {
			c.d.Logger.Info("ring ignored: bluetooth not ready", "phone_connected", c.tel != nil, "audio_routing", c.routingOK)
			return
		}
		c.startCall(ctx)
	default:
		if c.cur == nil {
			return
		}
		switch c.cur.state {
		case stateDialing:
			c.onTelDialing(c.cur, ev)
		case stateAnswering:
			c.onTelAnswering(c.cur, ev)
		}
	}
}
