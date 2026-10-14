package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/hfp"
	"github.com/volodymyrsmirnov/siptooth/internal/hooks"
	"github.com/volodymyrsmirnov/siptooth/internal/placeholder"
	"github.com/volodymyrsmirnov/siptooth/internal/sip"
)

// callState is the coordinator state of the current call (spec 4.1).
type callState int

const (
	stateIdle callState = iota
	stateDialing
	stateAnswering
	stateBridged
	stateEnding
)

func (s callState) String() string {
	switch s {
	case stateDialing:
		return "dialing"
	case stateAnswering:
		return "answering"
	case stateBridged:
		return "bridged"
	case stateEnding:
		return "ending"
	}
	return "idle"
}

// dialPhase is the progress inside stateDialing.
type dialPhase int

const (
	phaseCallerID  dialPhase = iota // waiting for +CLIP (spec 4.2 step 2)
	phasePreparing                  // Prepare running in the background
	phaseInviting                   // INVITE in flight
)

const (
	timerCallerID = "caller_id"
	timerDial     = "dial"
)

// activeCall is everything the coordinator knows about the current call.
// Only the loop goroutine touches it.
type activeCall struct {
	id    string
	gen   int
	state callState
	phase dialPhase

	ctx    context.Context // cancelled when the call ends
	cancel context.CancelFunc

	timerGen map[string]int
	ringAt   time.Time

	// Caller and the expanded INVITE ingredients (resolveIdentity).
	callerRaw    string
	callerType   int
	identityDone bool
	identityErr  error
	caller       string // cleaned number; empty when withheld
	withheld     bool
	target       string
	from         string
	displayName  string
	headers      map[string]string

	ringingEmitted bool

	prepared   *Prepared   // set once Prepare succeeded; owns the RTP Media
	attempt    sip.Attempt // while the INVITE result is pending
	sipCall    sip.Call    // once answered
	sipStat    int         // last final status of the INVITE
	sipRsn     string      // its reason phrase
	errText    string      // detail for call.ended error
	answerSent bool        // ATA was sent: the phone call is ours (spec 4.1)
}

// endSpec says how a call ends.
type endSpec struct {
	outcome  string
	hangupBy string
	errText  string
	// hangupPhone sends AT+CHUP in the background (only for a call siptooth
	// answered, spec 4.1).
	hangupPhone bool
	// farEndGone: the SIP side already ended, so no BYE is sent.
	farEndGone bool
}

// ---- Idle -> Dialing (spec 4.2) ----

func (c *Coordinator) startCall(ctx context.Context) {
	now := c.d.Clock.Now()
	c.nextGen++
	call := &activeCall{
		id: c.d.NewCallID(now), gen: c.nextGen, state: stateDialing, phase: phaseCallerID,
		ringAt: now, timerGen: map[string]int{},
	}
	call.ctx, call.cancel = context.WithCancel(ctx)
	c.cur = call
	c.d.Logger.Info("call ringing", "call_id", call.id)
	if wait := time.Duration(c.d.Config.Calls.CallerIDWaitMs) * time.Millisecond; wait > 0 {
		c.startTimer(call, timerCallerID, wait)
		return
	}
	c.beginDial(call)
}

func (c *Coordinator) onCallerIDWait(call *activeCall) {
	if call.state != stateDialing || call.phase != phaseCallerID {
		return
	}
	c.d.Logger.Info("caller id wait expired, caller withheld", "call_id", call.id)
	c.beginDial(call)
}

// beginDial leaves the caller-ID wait: expand the INVITE ingredients, emit
// call.ringing and start Prepare in the background (spec 4.2 steps 3-4).
func (c *Coordinator) beginDial(call *activeCall) {
	c.stopTimer(call, timerCallerID)
	c.resolveIdentity(call)
	c.emitRinging(call)
	if call.identityErr != nil {
		c.finish(call, endSpec{outcome: OutcomeSIPError, errText: call.identityErr.Error()})
		return
	}
	call.phase = phasePreparing
	// The dial timer covers Prepare and the INVITE together, so a stuck
	// Prepare cannot leave the phone ringing into a dead call.
	dial := time.Duration(c.d.Config.SIP.DialTimeoutSeconds) * time.Second
	if dial <= 0 {
		dial = defaultDialTimeout
	}
	c.startTimer(call, timerDial, dial)

	host := nextHopHost(c.d.Config.SIP.Proxy, call.target)
	gen := call.gen
	c.bg.Add(1)
	go func() {
		defer c.bg.Done()
		p, err := c.d.SIP.Prepare(call.ctx, host)
		if !c.post(msg{kind: mPrepared, gen: gen, prepared: p, err: err}) {
			closeMedia(c.d.Logger, p.Media)
		}
	}()
}

// resolveIdentity cleans the caller and expands target, From, display name
// and headers once per call (spec 4.7, 5.1).
func (c *Coordinator) resolveIdentity(call *activeCall) {
	if call.identityDone {
		return
	}
	call.identityDone = true
	call.caller, call.withheld = placeholder.CleanCaller(call.callerRaw, call.callerType)
	cfg := c.d.Config
	who := call.caller
	if call.withheld {
		who = cfg.SIP.AnonymousUser
	}
	v := placeholder.Values{Caller: who, Number: cfg.Phone.Number, CallID: call.id}

	var err error
	if call.target, err = placeholder.ExpandURI(cfg.SIP.Target, v); err != nil {
		call.identityErr = err
		return
	}
	if call.from, err = placeholder.ExpandURI(cfg.SIP.FromTemplate(), v); err != nil {
		call.identityErr = err
		return
	}
	if cfg.SIP.DisplayName != "" {
		call.displayName = placeholder.ExpandDisplayName(cfg.SIP.DisplayName, v)
	}
	if len(cfg.SIP.Headers) > 0 {
		call.headers = make(map[string]string, len(cfg.SIP.Headers))
		for k, tmpl := range cfg.SIP.Headers {
			if call.headers[k], err = placeholder.ExpandHeader(tmpl, v); err != nil {
				call.identityErr = fmt.Errorf("header %s: %w", k, err)
				return
			}
		}
	}
}

func (c *Coordinator) emitRinging(call *activeCall) {
	if call.ringingEmitted {
		return
	}
	call.ringingEmitted = true
	c.d.Hooks.Emit("call.ringing", c.callInfo(call))
}

// nextHopHost is the host Prepare decides the advertised address toward:
// the sip.proxy host, else the host of the expanded target (spec 5.2).
func nextHopHost(proxy, target string) string {
	hp := proxy
	if hp == "" {
		rest := target
		if len(rest) >= 4 && strings.EqualFold(rest[:4], "sip:") {
			rest = rest[4:]
		}
		if i := strings.IndexByte(rest, '?'); i >= 0 {
			rest = rest[:i]
		}
		if i := strings.LastIndexByte(rest, '@'); i >= 0 {
			rest = rest[i+1:]
		}
		if i := strings.IndexByte(rest, ';'); i >= 0 {
			rest = rest[:i]
		}
		hp = rest
	}
	if h, _, err := net.SplitHostPort(hp); err == nil {
		return h
	}
	return strings.Trim(hp, "[]")
}

// onPrepared handles the result of the background Prepare.
func (c *Coordinator) onPrepared(m msg) {
	call := c.current(m.gen)
	if call == nil || call.phase != phasePreparing {
		c.discard(m, "call over")
		return
	}
	p := m.prepared
	if m.err != nil {
		closeMedia(c.d.Logger, p.Media)
		c.finish(call, endSpec{outcome: OutcomeSIPError, errText: "prepare: " + m.err.Error()})
		return
	}
	if p.Media == nil {
		c.finish(call, endSpec{outcome: OutcomeSIPError, errText: "prepare: no media session"})
		return
	}
	call.prepared = &p

	cfg := c.d.Config.SIP
	offer := p.Offer
	if offer.LocalKey == nil && len(p.LocalKey) > 0 {
		offer.LocalKey = p.LocalKey
	}
	nextHop, transport := p.NextHop, p.Transport
	if nextHop == "" {
		nextHop = cfg.Proxy
	}
	if transport == "" {
		transport = cfg.Transport
	}
	log, id := c.d.Logger, call.id
	att, err := c.d.SIP.Start(sip.DialRequest{
		Target: call.target, From: call.from, DisplayName: call.displayName,
		Contact: p.Contact, Headers: call.headers, Offer: offer,
		NextHop: nextHop, Transport: transport,
		OnResponse: func(status int, reason string) {
			log.Info("sip response", "call_id", id, "status", status, "reason", reason)
		},
	})
	if err != nil {
		c.finish(call, endSpec{outcome: OutcomeSIPError, errText: "start invite: " + err.Error()})
		return
	}
	call.attempt = att
	call.phase = phaseInviting
	c.d.Logger.Info("invite sent", "call_id", call.id, "target", call.target)

	gen := call.gen
	go func() {
		select {
		case res := <-att.Result():
			if !c.post(msg{kind: mDial, gen: gen, result: res}) && res.Call != nil {
				c.byeNow(res.Call, id)
			}
		case <-c.done:
		}
	}()
}

// ---- Dialing events (spec 4.3) ----

func (c *Coordinator) onTelDialing(call *activeCall, ev hfp.Event) {
	switch ev.Kind {
	case hfp.CallerID:
		if call.phase == phaseCallerID {
			call.callerRaw, call.callerType = ev.Number, ev.NumberType
			c.beginDial(call)
		} // later +CLIP: the INVITE already went out without it
	case hfp.CallSetupCleared:
		c.finish(call, endSpec{outcome: OutcomeCallerAbandoned, hangupBy: hangupUndefined})
	case hfp.CallActive:
		if !call.answerSent {
			// The owner picked up on the handset; the phone call is never touched.
			c.finish(call, endSpec{outcome: OutcomeOwnerAnswered})
		}
	}
}

func (c *Coordinator) onDialTimeout(call *activeCall) {
	if call.state != stateDialing {
		return
	}
	c.finish(call, endSpec{outcome: OutcomeSIPTimeout})
}

func (c *Coordinator) onDialResult(m msg) {
	call := c.current(m.gen)
	if call == nil || call.attempt == nil {
		c.discard(m, "call over")
		return
	}
	call.attempt = nil
	res := m.result
	if res.Call != nil {
		c.enterAnswering(call, res)
		return
	}
	call.sipStat, call.sipRsn = res.Status, res.Reason
	detail := res.Reason
	if res.Err != nil {
		detail = res.Err.Error()
	}
	switch res.Cause {
	case sip.FailRejected:
		c.finish(call, endSpec{outcome: OutcomeSIPRejected})
	case sip.FailMedia:
		c.finish(call, endSpec{outcome: OutcomeSIPMediaFailed, errText: detail})
	case sip.FailError:
		c.finish(call, endSpec{outcome: OutcomeSIPError, errText: detail})
	default:
		c.finish(call, endSpec{outcome: OutcomeSIPError, errText: fmt.Sprintf("unexpected dial result %q: %s", res.Cause, detail)})
	}
}

// ---- Answering ----
//
// The answering handlers below are the skeleton Task 15 completes (media
// start, ATA, CallActive wait, SCO). Today an answered INVITE is held open
// and only the terminations that need no phone action are handled.

func (c *Coordinator) enterAnswering(call *activeCall, res sip.DialResult) {
	c.stopTimer(call, timerDial)
	call.state = stateAnswering
	call.sipCall = res.Call
	call.sipStat, call.sipRsn = res.Status, res.Reason
	c.d.Logger.Info("sip answered", "call_id", call.id, "sip_call_id", res.Call.SIPCallID(), "status", res.Status)

	sc, gen := res.Call, call.gen
	go func() {
		select {
		case cause := <-sc.Ended():
			c.post(msg{kind: mSIPEnded, gen: gen, cause: cause})
		case <-call.ctx.Done():
		case <-c.done:
		}
	}()
}

func (c *Coordinator) onSIPEnded(m msg) {
	call := c.current(m.gen)
	if call == nil || call.state != stateAnswering {
		c.discard(m, "call over")
		return
	}
	// Before CallActive the phone is left alone (spec 4.4).
	c.finish(call, endSpec{outcome: OutcomeFarEndEnded, hangupBy: HangupFarEnd, farEndGone: true,
		errText: "sip " + string(m.cause)})
}

func (c *Coordinator) onTelAnswering(call *activeCall, ev hfp.Event) {
	switch ev.Kind {
	case hfp.CallSetupCleared:
		c.finish(call, endSpec{outcome: OutcomeCallerAbandoned, hangupBy: HangupCaller})
	case hfp.CallActive:
		if !call.answerSent {
			c.finish(call, endSpec{outcome: OutcomeOwnerAnswered})
		}
	}
}

// ---- Ending (spec 4.6) ----

// finish ends call exactly once: it releases the attempt, the SIP dialog and
// the RTP session, emits the single call.ended, logs the summary line and
// returns to Idle. BYE and AT+CHUP run in the background, each bounded by 3 s,
// so Idle is never held up by them.
func (c *Coordinator) finish(call *activeCall, e endSpec) {
	if call.state == stateEnding {
		return
	}
	call.state = stateEnding
	call.cancel()

	if call.attempt != nil {
		call.attempt.Abandon() // returns at once; internal/sip finishes the transaction
		call.attempt = nil
	}
	if call.sipCall != nil && !e.farEndGone {
		c.hangupSIP(call.sipCall, call.id)
	}
	if e.hangupPhone && c.tel != nil {
		c.hangupPhone(c.tel, call.id)
	}

	var media *hooks.MediaStats
	var stats mediaCounters
	if call.prepared != nil {
		stats = readCounters(call.prepared.Media)
		media = &hooks.MediaStats{
			Sent: stats.Sent, Received: stats.Received, Lost: stats.Lost, Late: stats.Late,
			Duplicate: stats.Duplicate, Overflow: stats.Overflow,
			DiscardedSilence: stats.DiscardedSilence, SRTPFail: stats.SRTPFail,
		}
		closeMedia(c.d.Logger, call.prepared.Media)
	}

	// A call that ended before call.ringing went out still gets one, so every
	// call.ended has its call.ringing.
	c.resolveIdentity(call)
	c.emitRinging(call)

	ringing := c.d.Clock.Now().Sub(call.ringAt).Seconds()
	info := c.callInfo(call)
	info.Outcome, info.HangupBy, info.Error = e.outcome, e.hangupBy, e.errText
	info.SIPStatus, info.SIPReason = call.sipStat, call.sipRsn
	info.RingingSeconds = ringing
	info.Media = media
	c.d.Hooks.Emit("call.ended", info)

	caller := call.caller
	if call.withheld {
		caller = "withheld"
	}
	c.d.Logger.Info("call ended",
		"call_id", call.id, "caller", caller, "outcome", e.outcome, "hangup_by", e.hangupBy,
		"sip_status", call.sipStat, "ringing_s", ringing, "bridged_s", 0.0,
		"codec", info.Codec, "nat_mode", info.NATMode,
		"lost", stats.Lost, "late", stats.Late, "discarded_silence", stats.DiscardedSilence)

	c.lastOutcome = e.outcome
	c.cur = nil
}

// callInfo is the hook payload view of call as far as it is known.
func (c *Coordinator) callInfo(call *activeCall) hooks.CallInfo {
	info := hooks.CallInfo{
		ID: call.id, Number: c.d.Config.Phone.Number, Target: call.target,
		CallerWithheld: call.withheld,
	}
	if !call.withheld {
		v := call.caller
		info.Caller = &v
	}
	if p := call.prepared; p != nil {
		info.NATMode = string(p.NAT.Mode)
		if p.NAT.Media.IsValid() {
			info.AdvertisedMedia = p.NAT.Media.String()
		}
		info.SRTP = len(p.LocalKey) > 0 || len(p.Offer.LocalKey) > 0
	}
	if sc := call.sipCall; sc != nil {
		info.SIPCallID = sc.SIPCallID()
		rm := sc.Remote()
		info.Codec = rm.SendCodec.String()
		if rm.Addr.IsValid() {
			info.RemoteMedia = rm.Addr.String()
		}
		info.SRTP = rm.SRTP
	}
	return info
}

type mediaCounters struct {
	Sent, Received, Lost, Late, Duplicate, Overflow, DiscardedSilence, SRTPFail uint64
}

func readCounters(m Media) mediaCounters {
	s := m.Stats()
	return mediaCounters{Sent: s.Sent, Received: s.Received, Lost: s.Lost, Late: s.Late,
		Duplicate: s.Duplicate, Overflow: s.Overflow, DiscardedSilence: s.DiscardedSilence, SRTPFail: s.SRTPFail}
}

func closeMedia(log *slog.Logger, m Media) {
	if m == nil {
		return
	}
	if err := m.Close(); err != nil {
		log.Debug("closing media", "error", err)
	}
}

// hangupSIP sends BYE in the background, bounded by cleanupTimeout.
func (c *Coordinator) hangupSIP(sc sip.Call, callID string) {
	c.bg.Add(1)
	go func() {
		defer c.bg.Done()
		c.byeNow(sc, callID)
	}()
}

// byeNow sends BYE and waits at most cleanupTimeout.
func (c *Coordinator) byeNow(sc sip.Call, callID string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if err := sc.Hangup(ctx); err != nil && !errors.Is(err, context.Canceled) {
		c.d.Logger.Warn("BYE failed", "call_id", callID, "error", err)
	}
}

// hangupPhone sends AT+CHUP in the background, bounded by cleanupTimeout.
func (c *Coordinator) hangupPhone(t Telephony, callID string) {
	c.bg.Add(1)
	go func() {
		defer c.bg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if err := t.Hangup(ctx); err != nil {
			c.d.Logger.Warn("AT+CHUP failed", "call_id", callID, "error", err)
		}
	}()
}
